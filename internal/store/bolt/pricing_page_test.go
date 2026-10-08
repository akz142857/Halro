package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPricingPageAllRowsAndOffPageLifecycleMatchWholeTimeline(t *testing.T) {
	s, base := streamPriceFixture(t, 130, 0)
	ctx := context.Background()
	selected := base.Add(64 * time.Second)
	whole, err := s.ListDeploymentPriceVersions(ctx, "dep_stream")
	if err != nil {
		t.Fatal(err)
	}
	want, err := domain.DerivePriceLifecycle(whole, "dep_stream", selected)
	if err != nil {
		t.Fatal(err)
	}
	var cursor string
	count, pages := 0, 0
	for {
		page, err := s.DeploymentPriceVersionsPage(ctx, "dep_stream", cursor, 17, selected)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 17 || page.ActivePrice == nil || page.ActivePrice.ID != "price_stream_64" || page.NextScheduledPrice == nil || page.NextScheduledPrice.ID != "price_stream_65" || page.BlockingPrice == nil || page.BlockingPrice.ID != "price_stream_129" {
			t.Fatal(page)
		}
		for _, item := range page.Items {
			if item.ID != whole[count].ID || item.Status != want[item.ID] {
				t.Fatal(count, item, want[item.ID])
			}
			count++
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if count != 130 || pages != 8 {
		t.Fatal(count, pages)
	}
}

func TestPricingPageBoundsWideRowsAndRefusesConflictsOutsideVisiblePage(t *testing.T) {
	t.Run("wide", func(t *testing.T) {
		s, base := streamPriceFixture(t, 3, 512<<10)
		page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 100, base)
		if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
			t.Fatal(page, err)
		}
		next, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", page.NextCursor, 100, base)
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != "price_stream_1" {
			t.Fatal(next, err)
		}
	})
	t.Run("single-wide", func(t *testing.T) {
		s, base := streamPriceFixture(t, 1, 2<<20)
		page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 100, base)
		if err != nil || len(page.Items) != 1 || page.NextCursor != "" {
			t.Fatal(page, err)
		}
	})
	t.Run("off-page-duplicate", func(t *testing.T) {
		s, base := streamPriceFixture(t, 130, 0)
		if err := s.update(func(tx *Tx) error {
			bucket := tx.Bucket(bucketDeploymentPriceVersions)
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(bucket.Get([]byte("price_stream_129")), &price); err != nil {
				return err
			}
			price.Version = 130
			raw, err := json.Marshal(price)
			if err != nil {
				return err
			}
			return bucket.Put([]byte(price.ID), raw)
		}); err != nil {
			t.Fatal(err)
		}
		page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 1, base)
		if !errors.Is(err, domain.ErrPriceTimelineConflict) || len(page.Items) != 0 || page.ActivePrice != nil {
			t.Fatal(page, err)
		}
	})
}

func TestPricingPageCursorBindsRevisionDeploymentAndStoreLifetime(t *testing.T) {
	s, base := streamPriceFixture(t, 3, 0)
	page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 1, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"garbage", page.NextCursor[:len(page.NextCursor)-1] + "!"} {
		if _, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", cursor, 1, base); !errors.Is(err, ErrPricingPageCursor) {
			t.Fatal(err)
		}
	}
	if _, err := s.DeploymentPriceVersionsPage(context.Background(), "other", page.NextCursor, 1, base); !errors.Is(err, ErrPricingPageCursor) {
		t.Fatal(err)
	}
	if err := s.update(func(tx *Tx) error {
		bucket := tx.Bucket(bucketDeploymentPriceVersions)
		var price domain.DeploymentPriceVersion
		if err := json.Unmarshal(bucket.Get([]byte("price_stream_0")), &price); err != nil {
			return err
		}
		price.Source.Reference = "changed reference"
		raw, err := json.Marshal(price)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(price.ID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", page.NextCursor, 1, base); !errors.Is(err, ErrPricingPageChanged) {
		t.Fatal(err)
	}
	if _, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 1, base); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.pricingPageKey != ([32]byte{}) {
		t.Fatal("Close retained cursor secret")
	}
	reopened := openJournalledStore(t, path)
	if _, err := reopened.DeploymentPriceVersionsPage(context.Background(), "dep_stream", page.NextCursor, 1, base); !errors.Is(err, ErrPricingPageCursor) {
		t.Fatal("old process cursor survived reopen", err)
	}
}

func TestPricingPageSummariesSkipCancelledAndStatusesAdvanceWithoutFactChange(t *testing.T) {
	s, base := streamPriceFixture(t, 6, 0)
	cancelled := base.Add(-time.Minute)
	if err := s.update(func(tx *Tx) error {
		for _, i := range []int{2, 5} {
			id := fmt.Sprintf("price_stream_%d", i)
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(id)), &price); err != nil {
				return err
			}
			price.CancelledAt, price.CancelledBy = &cancelled, "admin_one"
			if err := price.Validate(); err != nil {
				return err
			}
			raw, err := json.Marshal(price)
			if err != nil {
				return err
			}
			if err := tx.Bucket(bucketDeploymentPriceVersions).Put([]byte(price.ID), raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", "", 2, base.Add(time.Second))
	if err != nil || page.ActivePrice.ID != "price_stream_1" || page.NextScheduledPrice.ID != "price_stream_3" || page.BlockingPrice.ID != "price_stream_4" {
		t.Fatal(page, err)
	}
	next, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_stream", page.NextCursor, 2, base.Add(3*time.Second))
	if err != nil || next.Revision != page.Revision || next.ActivePrice.ID != "price_stream_3" || next.Items[0].Status != domain.PriceLifecycleCancelled || next.Items[1].Status != domain.PriceLifecycleActive {
		t.Fatal(next, err)
	}
}

func TestPricingPageUsesOneSharedIOClaimAndRejectsBeforeReading(t *testing.T) {
	s, m, base := pricingBudgetFixture(t)
	if _, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_budget", "", 1, base); !errors.Is(err, resourcebudget.ErrPressure) || s.pricingPageKey != ([32]byte{}) {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	page, err := s.DeploymentPriceVersionsPage(context.Background(), "dep_budget", "", 1, base)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	if stats := m.Snapshot(); stats.Acquired[resourcebudget.IO] != 1 || stats.NormalClaims != 0 || stats.RecoveryClaims != 1 {
		t.Fatal("nested audit duplicated/leaked IO", stats)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.DeploymentPriceVersionsPage(ctx, "dep_budget", "", 1, base); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".halro-price-audit-*"))
	if err != nil || len(paths) != 0 || m.Snapshot().NormalClaims != 0 {
		t.Fatal(paths, err, m.Snapshot())
	}
}
