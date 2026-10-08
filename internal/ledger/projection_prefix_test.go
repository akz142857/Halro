package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func prefixBuildFixture(t *testing.T) (string, ChainReport, []Record) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	log := openChained(t, path, nil)
	for i := 1; i <= 30; i++ {
		for _, event := range projectionTestLifecycle(i, false) {
			if _, err := log.Append(context.Background(), event); err != nil {
				t.Fatal(err)
			}
		}
		if i == 6 || i == 16 {
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Roll(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	var records []Record
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error { records = append(records, r); return nil })
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	return path, report, records
}

func TestLedgerProjectionPrefixUsesOnlyAllowedFactsIncludingRollBoundaries(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	cuts := []ProjectionPrefix{{}, {Head: Watermark{Generation: 1}}}
	// Before/after a batch boundary, unfinished accepted/reserved request,
	// compressed sealed history, and both occupied and empty rolled generations.
	for _, i := range []int{0, 1, 29, 63, 64, 79, 80, 149} {
		r := records[i]
		cuts = append(cuts, ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash})
	}
	cuts = append(cuts, ProjectionPrefix{Head: Watermark{Generation: 2, Sequence: 30}, Hash: records[29].Hash}, ProjectionPrefix{Head: report.Head, Hash: report.ChainHash})
	for _, cut := range cuts {
		t.Run(fmt.Sprintf("%d/%d/%d", cut.Head.Generation, cut.Head.Offset, cut.Head.Sequence), func(t *testing.T) {
			ctx := context.Background()
			p, err := BuildLedgerProjectionPrefix(ctx, path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report, cut)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			head, hash, err := p.ReadPosition(ctx)
			if err != nil || head != cut.Head || hash != cut.Hash {
				t.Fatal(head, hash, err)
			}
			state := NewState()
			for _, r := range records {
				allowed := r.Sequence <= cut.Head.Sequence
				if allowed {
					if err := state.Apply(r); err != nil {
						t.Fatal(err)
					}
				}
				_, found, err := p.ReadEventPosition(ctx, r.Event.EventID)
				if err != nil || found != allowed {
					t.Fatalf("event %d allowed=%v found=%v err=%v", r.Sequence, allowed, found, err)
				}
			}
			for _, key := range []BalanceKey{{ProjectID: "prj_1", PeriodID: "2026-10-01", TimezoneVersion: 1}, {ProjectID: "prj_1", PeriodID: "2026-10-02", TimezoneVersion: 2}, {ProjectID: "prj_1", PeriodID: "2026-10-03", TimezoneVersion: 1}} {
				disk, err := p.ReadBalance(ctx, key)
				memory, memoryErr := state.ReadBalance(ctx, key)
				if err != nil || memoryErr != nil || !reflect.DeepEqual(disk, memory) {
					t.Fatal(key, disk, memory, err, memoryErr)
				}
			}
		})
	}
}

func TestLedgerProjectionPrefixRejectsUnprovedCutAndDiscardsCandidate(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	r := records[64]
	valid := ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
	cases := map[string]ProjectionPrefix{}
	bad := valid
	bad.Hash[0] ^= 1
	cases["wrong-hash"] = bad
	bad = valid
	bad.Head.Sequence++
	cases["wrong-sequence"] = bad
	bad = valid
	bad.Head.Offset--
	cases["middle-of-frame"] = bad
	bad = valid
	bad.Head.Generation++
	cases["wrong-generation"] = bad
	bad = valid
	bad.Head.Offset = -1
	cases["negative-offset"] = bad
	bad = valid
	bad.Head.Generation = 0
	cases["nonempty-genesis"] = bad
	bad = valid
	bad.Head.Sequence = report.Head.Sequence + 1
	cases["beyond-head"] = bad
	bad = ProjectionPrefix{Head: Watermark{Generation: 2, Sequence: 30}, Hash: r.Hash}
	cases["wrong-roll-hash"] = bad
	for name, cut := range cases {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "projection.db")
			if p, err := BuildLedgerProjectionPrefix(context.Background(), path, testChainKey, output, report, cut); !errors.Is(err, ErrProjectionCorrupt) || p != nil {
				t.Fatal(p, err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func TestLedgerProjectionPrefixAuthenticatesExcludedSuffix(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	r := records[1]
	cut := ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash}
	manifest, err := loadSegmentManifest(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	// This generation is wholly after the permitted prefix. Repairing the plain
	// checksum cannot hide tampering from the whole-source authentication pass.
	sealed := filepath.Join(filepath.Dir(path), manifest.Segments[1].File)
	tamperWithSealedFrame(t, sealed)
	repairManifestChecksum(t, filepath.Dir(path), 2, sealed)
	output := filepath.Join(t.TempDir(), "projection.db")
	if p, err := BuildLedgerProjectionPrefix(context.Background(), path, testChainKey, output, report, cut); !errors.Is(err, ErrTampered) || p != nil {
		t.Fatal(p, err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
