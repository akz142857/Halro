package bolt

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkPricingPendingCount(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("history_%d", n), func(b *testing.B) {
			s := openJournalledStore(b, filepath.Join(b.TempDir(), "metadata.db"))
			// Build the history fixture before timing; fixed-counter migration scans each
			// source once and intentionally remains O(history).
			if err := s.migrate(func(tx *Tx) error {
				for i := 0; i < n; i++ {
					intent := pendingIntentFixture(fmt.Sprintf("bench_%08d", i), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
					raw, err := json.Marshal(intent)
					if err != nil {
						return err
					}
					if err := tx.Bucket(bucketPricingAuditIntents).Put([]byte(intent.EventID), raw); err != nil {
						return err
					}
				}
				return tx.Bucket(bucketPricingAuditControl).Put(pricingPendingCountKey, encodeUint64(uint64(n)))
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.PendingPricingAuditIntentCount(context.Background())
				if err != nil || got != uint64(n) {
					b.Fatal(got, err)
				}
			}
		})
	}
}
