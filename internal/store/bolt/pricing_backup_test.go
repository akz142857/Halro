package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

type backupPricingReader struct {
	PricingReferenceReader
	pending  func(context.Context, ledger.ProjectionCursor, ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.PendingLease], error)
	settled  func(context.Context, ledger.ProjectionCursor, ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.SettledAttempt], error)
	lease    func(context.Context, string) (ledger.Record, bool, error)
	position func(context.Context) (ledger.Watermark, [32]byte, error)
}

func (r backupPricingReader) PendingLeasePage(ctx context.Context, c ledger.ProjectionCursor, l ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.PendingLease], error) {
	if r.pending != nil {
		return r.pending(ctx, c, l)
	}
	return r.PricingReferenceReader.PendingLeasePage(ctx, c, l)
}
func (r backupPricingReader) SettlementPage(ctx context.Context, c ledger.ProjectionCursor, l ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.SettledAttempt], error) {
	if r.settled != nil {
		return r.settled(ctx, c, l)
	}
	return r.PricingReferenceReader.SettlementPage(ctx, c, l)
}
func (r backupPricingReader) ReadLease(ctx context.Context, id string) (ledger.Record, bool, error) {
	if r.lease != nil {
		return r.lease(ctx, id)
	}
	return r.PricingReferenceReader.ReadLease(ctx, id)
}
func (r backupPricingReader) ReadPosition(ctx context.Context) (ledger.Watermark, [32]byte, error) {
	if r.position != nil {
		return r.position(ctx)
	}
	return r.PricingReferenceReader.ReadPosition(ctx)
}

func backupPricingFixture(t *testing.T, each int) (*Store, *ledger.LedgerProjection) {
	t.Helper()
	s, snapshot := pricingRecoveryFixture(t, each*2)
	path := filepath.Join(t.TempDir(), "ledger.wal")
	key := bytes.Repeat([]byte{0x52}, 32)
	log, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < each*2; i++ {
		base := ledger.Event{EventID: fmt.Sprint("accepted_", i), Kind: ledger.EventRequestAccepted, RequestID: fmt.Sprint("req_", i), ProjectID: "prj_recovery", PeriodID: "2026-10-03", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: snapshot.PricingSelectedAt}
		if _, err := log.Append(context.Background(), base); err != nil {
			t.Fatal(err)
		}
		base.Kind, base.EventID, base.AttemptID = ledger.EventReservationCreated, fmt.Sprint("lease_", i), fmt.Sprintf("att_%03d", i)
		base.ReservationMicrosUSD, base.LeaseMode, base.PriceSnapshot = ledger.MicrosUSD(1), ledger.LeaseModeMetered, &snapshot
		base.TokenGuardPricingViewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if _, err := log.Append(context.Background(), base); err != nil {
			t.Fatal(err)
		}
		if i >= each {
			base.Kind, base.EventID = ledger.EventAttemptStarted, fmt.Sprint("start_", i)
			if _, err := log.Append(context.Background(), base); err != nil {
				t.Fatal(err)
			}
			base.Kind, base.EventID, base.CommittedMicrosUSD = ledger.EventAttemptSettled, fmt.Sprint("settled_", i), ledger.MicrosUSD(1)
			base.Outcome = "success"
			if _, err := log.Append(context.Background(), base); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildLedgerProjection(context.Background(), path, key, filepath.Join(t.TempDir(), "projection.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if err := s.RecoverDeploymentPricePins(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestBackupPricingReferencesPageAllPendingSettledAndPins(t *testing.T) {
	s, p := backupPricingFixture(t, 130)
	before, err := s.PricingBackupState()
	if err != nil {
		t.Fatal(err)
	}
	pendingRows, settledRows, pendingPages, settledPages, lookups := 0, 0, 0, 0, 0
	// Writer acquisition/rollback inside every external read proves metadata
	// readers are released. No authoritative metadata mutation occurs here.
	probe := func() {
		done := make(chan error, 1)
		go func() {
			tx, err := s.db.Begin(true)
			if err == nil {
				err = tx.Rollback()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("metadata reader held across Ledger read")
		}
	}
	reader := backupPricingReader{PricingReferenceReader: p}
	reader.pending = func(ctx context.Context, c ledger.ProjectionCursor, l ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.PendingLease], error) {
		probe()
		page, err := p.PendingLeasePage(ctx, c, l)
		pendingPages++
		pendingRows += len(page.Items)
		if l.Rows != 64 || l.Bytes != 8<<20 || len(page.Items) > l.Rows || page.Bytes > l.Bytes {
			t.Fatal("invalid bounded pending page", l, page)
		}
		return page, err
	}
	reader.settled = func(ctx context.Context, c ledger.ProjectionCursor, l ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.SettledAttempt], error) {
		probe()
		page, err := p.SettlementPage(ctx, c, l)
		settledPages++
		settledRows += len(page.Items)
		return page, err
	}
	reader.lease = func(ctx context.Context, id string) (ledger.Record, bool, error) {
		probe()
		lookups++
		return p.ReadLease(ctx, id)
	}
	if err := s.ValidateDeploymentPriceReferences(context.Background(), reader); err != nil {
		t.Fatal(err)
	}
	if pendingRows != 130 || settledRows != 130 || pendingPages != 3 || settledPages != 3 || lookups != 260 {
		t.Fatal(pendingRows, settledRows, pendingPages, settledPages, lookups)
	}
	after, err := s.PricingBackupState()
	if err != nil || before != after {
		t.Fatal("validation changed metadata", before, after, err)
	}
}

func TestBackupPricingReferencesFailClosedWithoutMutation(t *testing.T) {
	s, p := backupPricingFixture(t, 2)
	sentinel := errors.New("controlled read failure")
	cases := []struct {
		name   string
		reader backupPricingReader
		want   error
	}{
		{"pending read", backupPricingReader{PricingReferenceReader: p, pending: func(context.Context, ledger.ProjectionCursor, ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.PendingLease], error) {
			return ledger.ProjectionPage[ledger.PendingLease]{}, sentinel
		}}, sentinel},
		{"settled read", backupPricingReader{PricingReferenceReader: p, settled: func(context.Context, ledger.ProjectionCursor, ledger.ProjectionPageLimits) (ledger.ProjectionPage[ledger.SettledAttempt], error) {
			return ledger.ProjectionPage[ledger.SettledAttempt]{}, sentinel
		}}, sentinel},
		{"lease read", backupPricingReader{PricingReferenceReader: p, lease: func(context.Context, string) (ledger.Record, bool, error) { return ledger.Record{}, false, sentinel }}, sentinel},
		{"position read", backupPricingReader{PricingReferenceReader: p, position: func(context.Context) (ledger.Watermark, [32]byte, error) {
			return ledger.Watermark{}, [32]byte{}, sentinel
		}}, sentinel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, _ := s.PricingBackupState()
			err := s.ValidateDeploymentPriceReferences(context.Background(), c.reader)
			after, _ := s.PricingBackupState()
			if !errors.Is(err, c.want) || before != after {
				t.Fatal(err, before, after)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ValidateDeploymentPriceReferences(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	head, hash, err := p.ReadPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	reader := backupPricingReader{PricingReferenceReader: p, position: func(context.Context) (ledger.Watermark, [32]byte, error) {
		calls++
		if calls > 1 {
			hash[0] ^= 1
		}
		return head, hash, nil
	}}
	if err := s.ValidateDeploymentPriceReferences(context.Background(), reader); !errors.Is(err, ErrPricingRecoveryChanged) {
		t.Fatal("changed projection accepted", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateDeploymentPriceReferences(context.Background(), p); !errors.Is(err, ledger.ErrProjectionUnavailable) {
		t.Fatal(err)
	}
}

func TestBackupPricingReferencesRejectMetadataInsertionBehindCursor(t *testing.T) {
	s, p := backupPricingFixture(t, 65)
	reader := backupPricingReader{PricingReferenceReader: p}
	calls := 0
	reader.lease = func(ctx context.Context, id string) (ledger.Record, bool, error) {
		calls++
		if calls == 65 {
			if err := s.update(func(tx *Tx) error {
				raw := bytes.Clone(tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_000")))
				return tx.Bucket(bucketDeploymentPricePins).Put([]byte("aaa_new"), raw)
			}); err != nil {
				return ledger.Record{}, false, err
			}
		}
		return p.ReadLease(ctx, id)
	}
	if err := s.ValidateDeploymentPriceReferences(context.Background(), reader); !errors.Is(err, ErrPricingRecoveryChanged) {
		t.Fatal("metadata insertion escaped validation", err)
	}
	if len(pricingRecoveryPins(t, s)) != 131 {
		t.Fatal("validator mutated pins")
	}
}

func TestBackupPricingReferencesRejectMissingPriceAndUnresolvedPin(t *testing.T) {
	for _, name := range []string{"price content", "missing price", "unresolved pin", "high-water key", "wide high-water"} {
		t.Run(name, func(t *testing.T) {
			s, p := backupPricingFixture(t, 1)
			err := s.update(func(tx *Tx) error {
				switch name {
				case "price content":
					raw := tx.Bucket(bucketDeploymentPriceVersions).Get([]byte("price_recovery"))
					var price domain.DeploymentPriceVersion
					if err := json.Unmarshal(raw, &price); err != nil {
						return err
					}
					price.Currency = "EUR"
					raw, err := json.Marshal(price)
					if err != nil {
						return err
					}
					return tx.Bucket(bucketDeploymentPriceVersions).Put([]byte(price.ID), raw)
				case "missing price":
					return tx.Bucket(bucketDeploymentPriceVersions).Delete([]byte("price_recovery"))
				case "unresolved pin":
					raw := tx.Bucket(bucketDeploymentPricePins).Get([]byte("att_000"))
					var pin domain.PricePinIntent
					if err := json.Unmarshal(raw, &pin); err != nil {
						return err
					}
					pin.State, pin.LedgerSequence, pin.CommittedAt = domain.PricePinPrepared, 0, nil
					raw, err := json.Marshal(pin)
					if err != nil {
						return err
					}
					return tx.Bucket(bucketDeploymentPricePins).Put([]byte(pin.AttemptID), raw)
				case "high-water key":
					return tx.Bucket(bucketDeploymentPricingHighWater).Put([]byte("wrong"), bytes.Clone(tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_recovery"))))
				case "wide high-water":
					raw := tx.Bucket(bucketDeploymentPricingHighWater).Get([]byte("dep_recovery"))
					var high domain.DeploymentPricingHighWater
					if err := json.Unmarshal(raw, &high); err != nil {
						return err
					}
					high.Quarantined = true
					high.QuarantineReason = strings.Repeat("x", pricingRecoveryBytes+1)
					raw, err := json.Marshal(high)
					if err != nil {
						return err
					}
					return tx.Bucket(bucketDeploymentPricingHighWater).Put([]byte(high.DeploymentID), raw)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := s.PricingBackupState()
			err = s.ValidateDeploymentPriceReferences(context.Background(), p)
			after, _ := s.PricingBackupState()
			if before != after {
				t.Fatal("validation mutated pricing")
			}
			if name == "wide high-water" {
				if err != nil {
					t.Fatal("legal wide row rejected", err)
				}
			} else if err == nil {
				t.Fatal("invalid references accepted")
			}
		})
	}
}
