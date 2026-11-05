package bolt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func freeSuspendPrice(id, deployment string, at time.Time) domain.DeploymentPriceVersion {
	p := newStoredPrice(id, deployment, at)
	p.BillingMode = domain.BillingModeFree
	p.InputMicrosPerMillion = 0
	p.CachedInputMicrosPerMillion = 0
	p.OutputMicrosPerMillion = 0
	p.FixedRequestMicrosUSD = 0
	return p
}

func TestUnchangedFreePriceSurvivesRepeatedVMObservationGaps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "metadata.db")
	s, err := openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	id := "dep_suspend_free"
	seedPricingDeployment(t, s, id, 0, 0, 0)
	wall := time.Now().UTC()
	observed := time.Now()
	p, err := s.CreateDeploymentPriceVersion(ctx, freeSuspendPrice("price_unchanged", id, wall.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, id, "att_before_suspend", wall, observed, time.Second, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	for index, gap := range []time.Duration{4064 * time.Second, 2 * time.Hour, 45 * time.Minute} {
		wall = wall.Add(gap + 5*time.Second)
		observed = observed.Add(5 * time.Second)
		price, snapshot, intent, err := s.PrepareDeploymentPricePin(ctx, id, string(rune('a'+index)), wall, observed, time.Second, 30*time.Second)
		if err != nil {
			t.Fatalf("resume %d quarantined unchanged free price: %v", index, err)
		}
		if price.ID != p.ID || snapshot.PriceVersionID != p.ID || intent.ClockForwardGapNanos != int64(gap) {
			t.Fatalf("price/gap changed: %+v %+v", price, intent)
		}
		if err := s.PricingReadiness(ctx); err != nil {
			t.Fatal(err)
		}
		// Normal activity after resuming must not repeatedly report the old gap.
		wall = wall.Add(time.Second)
		observed = observed.Add(time.Second)
		_, _, pin, err := s.PrepareDeploymentPricePin(ctx, id, string(rune('x'+index)), wall, observed, time.Second, 30*time.Second)
		if err != nil || pin.ClockForwardGapNanos != 0 {
			t.Fatalf("anchor not rebased: %+v %v", pin, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PrepareDeploymentPricePin(ctx, id, "att_restarted", wall.Add(time.Second), time.Now(), time.Second, 30*time.Second); err != nil {
		t.Fatalf("restarted free deployment blocked: %v", err)
	}
}

func TestVMGapExceptionCannotReleasePaidOrChangedPricing(t *testing.T) {
	for _, kind := range []string{"metered", "free-to-metered", "free-to-free", "free-rollback", "existing-quarantine"} {
		t.Run(kind, func(t *testing.T) {
			s, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			id := "dep_gap_guard"
			seedPricingDeployment(t, s, id, 0, 0, 0)
			now := time.Now().UTC()
			observed := time.Now()
			p := freeSuspendPrice("price_initial", id, now.Add(-time.Hour))
			if kind == "metered" {
				p = newStoredPrice("price_initial", id, now.Add(-time.Hour))
			}
			if _, err := s.CreateDeploymentPriceVersion(ctx, p); err != nil {
				t.Fatal(err)
			}
			if kind == "free-to-metered" || kind == "free-to-free" {
				next := newStoredPrice("price_next", id, now.Add(time.Minute))
				if kind == "free-to-free" {
					next = freeSuspendPrice("price_next", id, now.Add(time.Minute))
				}
				if _, err := s.CreateDeploymentPriceVersion(ctx, next); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, _, err := s.PrepareDeploymentPricePin(ctx, id, "att_before", now, observed, time.Second, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			selected := now.Add(4064 * time.Second)
			if kind == "free-rollback" || kind == "existing-quarantine" {
				selected = now.Add(-time.Minute)
			}
			if _, _, _, err := s.PrepareDeploymentPricePin(ctx, id, "att_blocked", selected, observed.Add(time.Second), time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
				t.Fatalf("%s escaped quarantine: %v", kind, err)
			}
			if kind == "existing-quarantine" {
				if _, _, _, err := s.PrepareDeploymentPricePin(ctx, id, "att_existing", now.Add(time.Hour), observed.Add(2*time.Second), time.Second, 30*time.Second); !errors.Is(err, ErrPricingQuarantined) {
					t.Fatalf("existing quarantine auto-cleared: %v", err)
				}
			}
		})
	}
}
