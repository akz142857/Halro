package limiter

import (
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// Keep this harness compatible with v0.5.0: the release comparison runs these
// exact bytes against both versions' synchronous admission paths.
func BenchmarkProjectAdmission(b *testing.B) {
	for _, limited := range []bool{false, true} {
		name := "unlimited"
		project := domain.Project{ID: "bench_project"}
		if limited {
			name = "limited"
			project.RPM, project.TPM, project.MaxConcurrency = 60_000, 6_000_000, 100
		}
		b.Run(name, func(b *testing.B) {
			manager := New()
			now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				at := now.Add(time.Duration(i) * time.Millisecond)
				lease, err := manager.Acquire(project, 64, at)
				if err != nil {
					b.Fatal(err)
				}
				if err := lease.Reconcile(48, at); err != nil {
					b.Fatal(err)
				}
				lease.Release()
			}
		})
	}
}

func BenchmarkProjectAdmissionContended(b *testing.B) {
	manager := New()
	project := domain.Project{ID: "shared_project", MaxConcurrency: 10_000}
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lease, err := manager.Acquire(project, 64, now)
			if err != nil {
				b.Fatal(err)
			}
			if err := lease.Reconcile(48, now); err != nil {
				b.Fatal(err)
			}
			lease.Release()
		}
	})
}
