package bolt

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
)

func BenchmarkProposalPageGrowth(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("history_%d", n), func(b *testing.B) {
			s := openJournalledStore(b, filepath.Join(b.TempDir(), "metadata.db"))
			if err := s.update(func(tx *Tx) error {
				return tx.Bucket(bucketDeployments).Put([]byte("dep_proposals"), []byte(`{"id":"dep_proposals"}`))
			}); err != nil {
				b.Fatal(err)
			}
			seedProposalPageFixture(b, s, "dep_proposals", n)
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			b.ReportAllocs()
			b.ResetTimer()
			var page DeploymentPriceProposalPage
			for i := 0; i < b.N; i++ {
				var err error
				page, err = s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 50)
				if err != nil || len(page.Items) != 50 {
					b.Fatal(len(page.Items), err)
				}
			}
			b.StopTimer()
			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			runtime.KeepAlive(page)
			b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained_delta_B")
			b.ReportMetric(float64(len(page.Items)), "rows")
		})
	}
}

// Warm native pages exclude fixture construction and the required cold audit.
func BenchmarkProposalIndexedWarmPageGrowth(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("history_%d", n), func(b *testing.B) {
			s := openJournalledStore(b, filepath.Join(b.TempDir(), "metadata.db"))
			if err := s.update(func(tx *Tx) error {
				return tx.Bucket(bucketDeployments).Put([]byte("dep_proposals"), []byte(`{"id":"dep_proposals"}`))
			}); err != nil {
				b.Fatal(err)
			}
			seedProposalPageFixture(b, s, "dep_proposals", n)
			first, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", "", 50)
			if err != nil || first.NextCursor == "" {
				b.Fatal(first, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				page, err := s.DeploymentPriceProposalsPage(context.Background(), "dep_proposals", first.NextCursor, 50)
				if err != nil || len(page.Items) != 50 {
					b.Fatal(page, err)
				}
			}
			b.StopTimer()
		})
	}
}
