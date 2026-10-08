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
)

func restorePricingFixture(t *testing.T, deployments, versions int) (*Store, time.Time) {
	t.Helper()
	s, err := OpenForWholeFilePublish(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	backupAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	err = s.migrate(func(tx *Tx) error {
		for d := 0; d < deployments; d++ {
			deployment := fmt.Sprintf("dep-%03d", d)
			for version := 0; version < versions; version++ {
				price := newStoredPrice(fmt.Sprintf("price-%03d-%03d", d, version), deployment, backupAt.Add(time.Duration(version+1)*time.Minute))
				price.Version, price.Revision = uint64(version+1), 1
				// A cancellation before backup excludes the last scheduled version;
				// cancellation after backup still requires confirmation on restore.
				if version == versions-1 || version == versions-2 {
					cancelled := backupAt.Add(-time.Minute)
					if version == versions-2 {
						cancelled = backupAt.Add(time.Minute)
					}
					price.CancelledAt, price.CancelledBy = &cancelled, "admin_one"
				}
				if err := putDeploymentPriceVersionTx(tx, price); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	return s, backupAt
}

func TestRestorePricingFiniteNativePagesPreserveLatestAndExistingHighWater(t *testing.T) {
	s, backupAt := restorePricingFixture(t, 70, 130)
	defer s.Close()
	preserved := domain.DeploymentPricingHighWater{DeploymentID: "dep-000", LatestObservedPriceVersionID: "old-version", LatestSelectedAt: backupAt, LatestObservedEffectiveFrom: backupAt.Add(-time.Hour), Revision: 7}
	if err := s.migrate(func(tx *Tx) error {
		raw, err := json.Marshal(preserved)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketDeploymentPricingHighWater).Put([]byte(preserved.DeploymentID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	count, err := s.QuarantineRestoredScheduledPrices(context.Background(), backupAt, backupAt.Add(4*time.Hour))
	if err != nil || count != 70 {
		t.Fatal(count, err)
	}
	if err := s.view(func(tx *Tx) error {
		for d := 0; d < 70; d++ {
			id := fmt.Sprintf("dep-%03d", d)
			var high domain.DeploymentPricingHighWater
			if err := json.Unmarshal(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte(id)), &high); err != nil {
				return err
			}
			if !high.Quarantined {
				return errors.New("scheduled deployment escaped quarantine")
			}
			if d == 0 {
				if high.LatestObservedPriceVersionID != preserved.LatestObservedPriceVersionID || high.Revision != 8 || !high.LatestSelectedAt.Equal(preserved.LatestSelectedAt) {
					return errors.New("existing high-water history was replaced")
				}
			} else if high.LatestObservedPriceVersionID != fmt.Sprintf("price-%03d-%03d", d, 128) {
				return fmt.Errorf("latest due version=%s", high.LatestObservedPriceVersionID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := s.QuarantineRestoredScheduledPrices(context.Background(), backupAt, backupAt.Add(4*time.Hour)); err != nil || count != 0 {
		t.Fatal("retry not idempotent", count, err)
	}
}

func TestRestorePricingRejectsUnqualifiedIndexAndChangedSourceBeforeMutation(t *testing.T) {
	for _, mode := range []string{"omitted", "orphan", "identity", "changed"} {
		t.Run(mode, func(t *testing.T) {
			s, backupAt := restorePricingFixture(t, 2, 3)
			defer s.Close()
			if mode != "changed" {
				if err := s.migrate(func(tx *Tx) error {
					timeline := tx.Bucket(bucketDeploymentPriceTimeline).Bucket([]byte("dep-001"))
					switch mode {
					case "omitted":
						return timeline.Delete(deploymentPriceTimelineKey(backupAt.Add(time.Minute)))
					case "orphan":
						return timeline.Put(deploymentPriceTimelineKey(backupAt.Add(5*time.Minute)), []byte("missing"))
					case "identity":
						return timeline.Put(deploymentPriceTimelineKey(backupAt.Add(time.Minute)), []byte("price-000-000"))
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			phase := func(name string) error {
				if mode == "changed" && name == "qualified" {
					return s.migrate(func(tx *Tx) error {
						return tx.Bucket(bucketAdminUsers).Put([]byte("later"), []byte(`{"username":"later"}`))
					})
				}
				return nil
			}
			if count, err := s.quarantineRestoredScheduledPricesWithPhase(context.Background(), backupAt, backupAt.Add(time.Hour), phase); err == nil || count != 0 {
				t.Fatal(count, err)
			}
			if err := s.view(func(tx *Tx) error {
				key, _ := tx.Bucket(bucketDeploymentPricingHighWater).Cursor().First()
				if key != nil {
					return errors.New("unqualified source mutated high-water")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestorePricingInterruptedCommittedPrefixRemainsRetryable(t *testing.T) {
	s, backupAt := restorePricingFixture(t, 3, 3)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	count, err := s.quarantineRestoredScheduledPricesWithPhase(ctx, backupAt, backupAt.Add(time.Hour), func(phase string) error {
		if phase == "high_water_committed" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatal(count, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen the actual native candidate; no operation-local map or progress key
	// can carry qualification across this interrupted process boundary.
	reopened, err := OpenForWholeFilePublish(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if count, err := reopened.QuarantineRestoredScheduledPrices(context.Background(), backupAt, backupAt.Add(time.Hour)); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
