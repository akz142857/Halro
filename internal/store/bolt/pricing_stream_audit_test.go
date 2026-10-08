package bolt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
)

func streamPriceFixture(t testing.TB, count, width int) (*Store, time.Time) {
	t.Helper()
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	seedPricingDeployment(t, s, "dep_stream", 0, 0, 0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for start := 0; start < count; start += 64 {
		if err := s.update(func(tx *Tx) error {
			for i := start; i < count && i < start+64; i++ {
				price := newStoredPrice(fmt.Sprintf("price_stream_%d", i), "dep_stream", base.Add(time.Duration(i)*time.Second))
				price.Version, price.Revision = uint64(count-i), 1 // unique, deliberately not time ordered
				noteWidth := width
				if noteWidth > 1024 {
					noteWidth = 1024
				}
				price.Source.Note = strings.Repeat("r", noteWidth)
				if err := putDeploymentPriceVersionTx(tx, price); err != nil {
					return err
				}
				if width > 1024 {
					// Opaque future/legacy JSON fields are ignored by the domain
					// decoder, but must still be bounded before cloning the row.
					raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(price.ID))
					object := map[string]json.RawMessage{}
					if err := json.Unmarshal(raw, &object); err != nil {
						return err
					}
					padding, _ := json.Marshal(strings.Repeat("r", width-1024))
					object["opaque_extension"] = padding
					raw, err := json.Marshal(object)
					if err != nil {
						return err
					}
					if err := tx.Bucket(bucketDeploymentPriceVersions).Put([]byte(price.ID), raw); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s, base
}

func TestPricingAuditIndexExactMembershipDepthFaultsAndOwnership(t *testing.T) {
	x, err := newPriceAuditIndex(filepath.Join(t.TempDir(), "versions"))
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	ctx := context.Background()
	// Each power-of-two adds one bit to the compressed path; UInt64Max also
	// exercises sparse/non-small version numbers without a history-sized map.
	for bit := 0; bit < 64; bit++ {
		if err := x.add(ctx, uint64(1)<<bit); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.add(ctx, ^uint64(0)); err != nil {
		t.Fatal(err)
	}
	if err := x.add(ctx, uint64(1)<<63); !errors.Is(err, domain.ErrPriceTimelineConflict) {
		t.Fatal(err)
	}
	size := x.size
	x.max = size
	if err := x.add(ctx, 3); !errors.Is(err, ErrPricingAuditLimit) || x.size != size {
		t.Fatal(err, x.size, size)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := x.add(canceled, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := x.file.WriteAt([]byte{0xff}, int64(x.root)+24); err != nil {
		t.Fatal(err)
	}
	if err := x.add(ctx, 2); !errors.Is(err, ErrPricingAuditCorrupt) {
		t.Fatal("corrupt node certified membership", err)
	}

	y, err := newPriceAuditIndex(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	path := y.file.Name()
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := y.close(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatal(string(data), err)
	}
}

func TestPricingStreamAuditPagesAndNativeSelectionKeepFullHistory(t *testing.T) {
	s, base := streamPriceFixture(t, 257, 512)
	page, err := s.priceAuditPage(context.Background(), "dep_stream", nil, nil)
	if err != nil || len(page.rows) != 64 || page.done {
		t.Fatal(len(page.rows), page.done, err)
	}
	if s.db.Stats().OpenTxN != 0 {
		t.Fatal("price page retained mapped reader")
	}
	for _, offset := range []int{0, 63, 64, 128, 256, 300} {
		price, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base.Add(time.Duration(offset)*time.Second))
		want := offset
		if want > 256 {
			want = 256
		}
		if err != nil || price.ID != fmt.Sprintf("price_stream_%d", want) {
			t.Fatal(offset, price.ID, err)
		}
	}
	if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base.Add(-time.Second)); !errors.Is(err, domain.ErrPriceUnavailable) {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".halro-price-audit-*"))
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
}

func TestPricingStreamAuditRejectsCrossPageDuplicateAndIndexBindings(t *testing.T) {
	for _, fault := range []string{"duplicate", "deployment", "id", "effective"} {
		t.Run(fault, func(t *testing.T) {
			s, base := streamPriceFixture(t, 130, 0)
			if err := s.update(func(tx *Tx) error {
				key := []byte("price_stream_129")
				prices := tx.Bucket(bucketDeploymentPriceVersions)
				var price domain.DeploymentPriceVersion
				if err := json.Unmarshal(prices.Get(key), &price); err != nil {
					return err
				}
				switch fault {
				case "duplicate":
					price.Version = 130
				case "deployment":
					price.DeploymentID = "dep_other"
				case "id":
					price.ID = "price_other"
				case "effective":
					price.EffectiveFrom = base.Add(127 * time.Second)
				}
				raw, err := json.Marshal(price)
				if err != nil {
					return err
				}
				return prices.Put(key, raw)
			}); err != nil {
				t.Fatal(err)
			}
			for retry := 0; retry < 2; retry++ {
				price, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base.Add(150*time.Second))
				if !errors.Is(err, domain.ErrPriceTimelineConflict) || price.ID != "" {
					t.Fatal(price.ID, err)
				}
			}
			if s.deploymentPricingState("dep_stream").timelineLoaded {
				t.Fatal("failed audit marked usable")
			}
			paths, _ := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".halro-price-audit-*"))
			if len(paths) != 0 {
				t.Fatal(paths)
			}
		})
	}
}

func TestPricingStreamAuditWideRowAndChangedPrefix(t *testing.T) {
	s, base := streamPriceFixture(t, 1, 2<<20)
	page, err := s.priceAuditPage(context.Background(), "dep_stream", nil, nil)
	if err != nil || len(page.rows) != 1 || len(page.rows[0].raw) <= priceAuditPageBytes {
		t.Fatal(err, len(page.rows))
	}
	if _, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(tx *Tx) error { return tx.Bucket(bucketMeta).Put(keyRuntimeSettings, []byte("changed")) }); err != nil {
		t.Fatal(err)
	}
	if page, err := s.priceAuditPage(context.Background(), "dep_stream", nil, &page.revision); err != nil || len(page.rows) != 1 {
		t.Fatal(err, len(page.rows))
	}
}

type priceAuditCancelContext struct {
	context.Context
	parent string
	stop   context.CancelFunc
}

func (c priceAuditCancelContext) Err() error {
	paths, _ := filepath.Glob(filepath.Join(c.parent, ".halro-price-audit-*", "versions"))
	if len(paths) > 0 {
		c.stop()
	}
	return c.Context.Err()
}

func TestPricingStreamAuditCancellationAfterPhysicalIndexCleansScratch(t *testing.T) {
	s, _ := streamPriceFixture(t, 130, 0)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ctx = priceAuditCancelContext{Context: ctx, parent: filepath.Dir(s.path), stop: stop}
	if _, err := s.auditDeploymentTimeline(ctx, "dep_stream"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".halro-price-audit-*"))
	if len(paths) != 0 {
		t.Fatal(paths)
	}
}

type priceAuditChangeContext struct {
	context.Context
	store   *Store
	changed bool
	err     error
	mutate  func(*Tx) error
}

func (c *priceAuditChangeContext) Err() error {
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(c.store.path), ".halro-price-audit-*", "versions"))
	// The source reader must already be released before an external write can
	// run here. This injects a real mutation after a copied page, not a fabricated
	// expected transaction number.
	if !c.changed && len(paths) > 0 && c.store.db.Stats().OpenTxN == 0 {
		c.changed = true
		c.err = c.store.update(c.mutate)
	}
	return c.err
}

func TestPricingStreamAuditActualMutationBetweenPagesRefusesMixedPrefix(t *testing.T) {
	s, _ := streamPriceFixture(t, 130, 0)
	ctx := &priceAuditChangeContext{Context: context.Background(), store: s, mutate: func(tx *Tx) error {
		return tx.Bucket(bucketDeploymentPriceVersions).Delete([]byte("price_stream_0"))
	}}
	if _, err := s.auditDeploymentTimeline(ctx, "dep_stream"); !errors.Is(err, ErrPricingRecoveryChanged) || !ctx.changed || ctx.err != nil {
		t.Fatal(err, ctx.changed, ctx.err)
	}
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".halro-price-audit-*"))
	if len(paths) != 0 {
		t.Fatal(paths)
	}
}

func TestPricingStreamAuditReadOnlySourceAndClosedSelection(t *testing.T) {
	s, base := streamPriceFixture(t, 130, 0)
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if price, err := s.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base); err == nil || price.ID != "" {
		t.Fatal(price.ID, err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	price, err := readOnly.SelectDeploymentPriceVersion(context.Background(), "dep_stream", base.Add(129*time.Second))
	if err != nil || price.ID != "price_stream_129" {
		t.Fatal(price.ID, err)
	}
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".halro-price-audit-*"))
	if len(paths) != 0 {
		t.Fatal("readonly diagnostic created source scratch", paths)
	}
}
