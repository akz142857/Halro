package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestVerifyAuditAnchorsAgreesDisagreesAndReportsTruncation(t *testing.T) {
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
	if summary.Records == 0 {
		t.Fatal("expected at least one audit record from startup/shutdown")
	}

	agreeing := boltstore.AuditAnchor{Sequence: 1, Records: summary.Records, LastHash: summary.LastHash, InstanceID: "ins_test"}
	forgedHash := summary.LastHash
	forgedHash[0] ^= 0xff
	disagreeing := boltstore.AuditAnchor{Sequence: 2, Records: summary.Records, LastHash: forgedHash, InstanceID: "ins_test"}
	truncated := boltstore.AuditAnchor{Sequence: 3, Records: summary.Records + 5, LastHash: summary.LastHash, InstanceID: "ins_test"}

	verdicts, err := VerifyAuditAnchors(context.Background(), cfg, []boltstore.AuditAnchor{agreeing, disagreeing, truncated})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 3 {
		t.Fatalf("verdicts=%#v", verdicts)
	}
	if verdicts[0].Outcome != AnchorVerdictAgree {
		t.Fatalf("agreeing anchor verdict=%q, want %q", verdicts[0].Outcome, AnchorVerdictAgree)
	}
	if verdicts[1].Outcome != AnchorVerdictDisagree {
		t.Fatalf("disagreeing anchor verdict=%q, want %q", verdicts[1].Outcome, AnchorVerdictDisagree)
	}
	if verdicts[2].Outcome != AnchorVerdictTruncated {
		t.Fatalf("over-claiming anchor verdict=%q, want %q", verdicts[2].Outcome, AnchorVerdictTruncated)
	}
}

// An anchor is meant to be the one claim about the chain that cannot be
// manufactured on the host: producing an agreeing one requires the audit key.
// Records: 0 with an all-zero LastHash needed neither — it named a position no
// record occupies, the hash lookup returned the zero value for the absent key,
// and the two compared equal. Anyone able to append a line to the witness file
// could mint an "agree" and pad a report with them.
func TestVerifyAuditAnchorsRefusesAnAnchorAtAPositionNoRecordOccupies(t *testing.T) {
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

	forged := boltstore.AuditAnchor{Sequence: 1, Records: 0, InstanceID: "ins_test"}
	verdicts, err := VerifyAuditAnchors(context.Background(), cfg, []boltstore.AuditAnchor{forged})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) == 0 || verdicts[0].Outcome != AnchorVerdictDisagree {
		t.Fatalf("forged zero-record anchor verdict=%#v, want the first to be %q", verdicts, AnchorVerdictDisagree)
	}
}

func TestVerifyAuditAnchorsDetectsARewrittenChainWithARecomputedTail(t *testing.T) {
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
	// The anchor was taken honestly before the rewrite; it must still
	// disagree with whatever the chain says now, even if what the chain says
	// now is itself self-consistent.
	honestAnchor := boltstore.AuditAnchor{Sequence: 1, Records: summary.Records, LastHash: summary.LastHash, InstanceID: "ins_test"}

	// Simulate an operator with filesystem access truncating the log to one
	// record short and letting the process re-derive from there — this stays
	// self-consistent from the chain's own point of view (audit.Verify would
	// not complain), which is exactly why an external anchor is the thing
	// that catches it, not another internal check.
	raw, err := os.ReadFile(cfg.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	// Truncate at a byte offset guaranteed to remove at least the final
	// frame's tail without knowing exact frame boundaries: cut a large
	// fraction of the file. This is only meant to change the observable
	// summary, not to stay a well-formed file — VerifyAuditAnchors only
	// needs the truncated file to open and replay, which a byte-aligned cut
	// followed by the log's own tail-recovery guarantees.
	cut := len(raw) / 2
	if err := os.WriteFile(cfg.AuditPath(), raw[:cut], 0o600); err != nil {
		t.Fatal(err)
	}

	verdicts, err := VerifyAuditAnchors(context.Background(), cfg, []boltstore.AuditAnchor{honestAnchor})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].Outcome == AnchorVerdictAgree {
		t.Fatalf("verdicts=%#v — a shortened chain must not agree with a pre-shortening anchor", verdicts)
	}
}

func TestLoadAuditAnchorsFileParsesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchors.jsonl")
	content := `{"sequence":1,"records":10,"instance_id":"ins_1","observed_at":"2026-08-06T00:00:00Z"}
{"sequence":2,"records":20,"instance_id":"ins_1","observed_at":"2026-08-06T00:05:00Z"}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	anchors, err := LoadAuditAnchorsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 2 || anchors[0].Sequence != 1 || anchors[1].Records != 20 {
		t.Fatalf("anchors=%#v", anchors)
	}
}

// The witness rotates its file once it reaches its cap. Reading only the live
// half would report every sequence in the retired half as a gap — the report
// would call a complete record incomplete, which is the same wrong answer as
// calling an incomplete one complete.
func TestLoadAuditAnchorsFileReadsTheRotatedGenerationFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchors.jsonl")
	retired := `{"sequence":1,"records":10,"instance_id":"ins_1","observed_at":"2026-08-06T00:00:00Z"}
{"sequence":2,"records":20,"instance_id":"ins_1","observed_at":"2026-08-06T00:05:00Z"}
`
	live := `{"sequence":3,"records":30,"instance_id":"ins_1","observed_at":"2026-08-06T00:10:00Z"}
`
	if err := os.WriteFile(path+rotatedAnchorSuffix, []byte(retired), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	anchors, err := LoadAuditAnchorsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]uint64, 0, len(anchors))
	for _, anchor := range anchors {
		got = append(got, anchor.Sequence)
	}
	if !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("sequences=%v, want [1 2 3] — the retired generation must come first", got)
	}

	// A missing live file is still an error: it is the path the operator named.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuditAnchorsFile(path); err == nil {
		t.Fatal("a missing live anchors file was accepted because a rotated one existed")
	}
}
