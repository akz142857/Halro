package tokenguard

import (
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
)

func BenchmarkAdmit(b *testing.B) {
	for _, test := range []struct {
		name   string
		policy domain.TokenGuardPolicy
	}{
		{name: "fixed", policy: domain.TokenGuardPolicy{ID: "guard", Name: "Guard", Enabled: true, Action: "observe"}},
		{name: "ewma", policy: domain.TokenGuardPolicy{
			ID: "guard", Name: "Guard", Enabled: true, Action: "observe",
			EWMAEnabled: true, EWMAAlpha: 0.2, EWMAMultiplier: 3,
			EWMAMinimumSamples: 100, EWMAWarmup: time.Hour,
			EWMAEvaluationWindow: time.Minute, EWMACooldown: time.Minute,
			EWMAAbsoluteRPM: 100,
		}},
	} {
		b.Run(test.name, func(b *testing.B) {
			manager, err := New([]domain.TokenGuardPolicy{test.policy})
			if err != nil {
				b.Fatal(err)
			}
			input := Input{
				PolicyID: "guard", ProjectID: "project", KeyID: "key",
				EstimatedTokens: 100, EstimatedCostMicrosUSD: 10,
				Now: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				manager.Admit(input)
			}
		})
	}
}

func BenchmarkAcquireReleaseContended(b *testing.B) {
	manager, err := New([]domain.TokenGuardPolicy{{ID: "guard", Name: "guard", Enabled: true, Action: "alert", Concurrency: 10_000}})
	if err != nil {
		b.Fatal(err)
	}
	input := Input{PolicyID: "guard", ProjectID: "project", KeyID: "key", Now: time.Now()}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			decision, lease := manager.Acquire(input)
			if !decision.Allowed {
				b.Fatal("unexpected rejection")
			}
			lease.Release()
		}
	})
}
