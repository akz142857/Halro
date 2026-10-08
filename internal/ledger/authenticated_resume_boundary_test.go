package ledger

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLedgerAuthenticatedResumeEmptyAndOffsetZeroBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.wal")
		log := openChained(t, path, nil)
		defer log.Close()
		for _, anchor := range []ReplayAnchor{{}, {Head: Watermark{Generation: 1}}} {
			count := 0
			head, err := log.ReplayAuthenticated(ctx, anchor, func(Record) error { count++; return nil })
			if err != nil || head != (Watermark{Generation: 1}) || count != 0 {
				t.Fatal(head, count, err)
			}
		}
		if _, err := log.Append(ctx, validReservation("evt_empty", "att_empty")); err != nil {
			t.Fatal(err)
		}
		for _, anchor := range []ReplayAnchor{{}, {Head: Watermark{Generation: 1}}} {
			var actual Record
			count := 0
			head, err := log.ReplayAuthenticated(ctx, anchor, func(r Record) error { actual = r; count++; return nil })
			nativeHead, nativeHash, _ := log.ChainHead()
			if err != nil || count != 1 || head != nativeHead || actual.Hash != nativeHash || actual.Hash == ([32]byte{}) {
				t.Fatal(head, count, actual, err)
			}
		}
	})
	t.Run("rolled-zero", func(t *testing.T) {
		path, report, records := prefixBuildFixture(t)
		log := openChained(t, path, nil)
		defer log.Close()
		for _, cut := range []ProjectionPrefix{{Head: Watermark{Generation: 2, Sequence: 30}, Hash: records[29].Hash}, {Head: Watermark{Generation: 3, Sequence: 80}, Hash: records[79].Hash}, {Head: report.Head, Hash: report.ChainHash}} {
			p, err := BuildLedgerProjectionPrefix(ctx, path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report, cut)
			if err != nil {
				t.Fatal(err)
			}
			anchor, err := p.ReadReplayAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			head, err := log.ReplayAuthenticated(ctx, anchor, func(r Record) error {
				count++
				if r.Hash == ([32]byte{}) {
					t.Fatal(r)
				}
				return p.Apply(ctx, r)
			})
			if err != nil || head != report.Head || count != len(records)-int(cut.Head.Sequence) {
				t.Fatal(head, count, err)
			}
			_ = p.Close()
		}
	})
}
