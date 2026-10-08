package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	bbolt "go.etcd.io/bbolt"
)

func TestProjectionReservationEvidenceMustAgreeBeforePagingOrApplying(t *testing.T) {
	for _, mutation := range []string{"amount", "key", "lease", "evidence"} {
		for _, api := range []string{"pending-page", "request-page", "pricing-view", "recovery-view", "apply", "authenticated-advance"} {
			t.Run(mutation+"/"+api, func(t *testing.T) {
				p, native, ref, directory := recoveryViewFixture(t)
				ctx := context.Background()
				key := projectionIDKey("att_000004")
				var row projectionReservation
				if err := ref.db.View(func(tx *bbolt.Tx) error {
					var found bool
					var err error
					row, found, err = projectionGet[projectionReservation](tx, "pending", key)
					if !found {
						return ErrProjectionCorrupt
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "amount":
					row.Reservation.Amount-- // Previously left a phantom reserved balance.
				case "key":
					row.Reservation.Key.TimezoneVersion++
				case "lease":
					row.Reservation.Lease.OccurredAt = row.Reservation.Lease.OccurredAt.Add(time.Second)
				case "evidence":
					row.Evidence.Event.OccurredAt = row.Evidence.Event.OccurredAt.Add(time.Second)
				}
				auth := api == "pricing-view" || api == "recovery-view" || api == "authenticated-advance"
				if auth {
					mutatePoint(t, p, "pending", key, row)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, "pending", key, row) }); err != nil {
					t.Fatal(err)
				}
				limits := ProjectionPageLimits{Rows: 64, Bytes: 8 << 20}
				var err error
				switch api {
				case "pending-page":
					page, e := ref.PendingLeasePage(ctx, ProjectionCursor{}, limits)
					err = e
					if !reflect.DeepEqual(page, ProjectionPage[PendingLease]{}) {
						t.Error("corrupt pending page escaped", page)
					}
				case "request-page":
					page, e := ref.RequestAttemptPage(ctx, "req_000004", ProjectionCursor{}, limits)
					err = e
					if !reflect.DeepEqual(page, ProjectionPage[ProjectedAttempt]{}) {
						t.Error("corrupt request page escaped", page)
					}
				case "pricing-view":
					err = p.WithFreshPricingView(ctx, func(v AuthenticatedPricingView) error {
						page, e := v.PendingLeasePage(ctx, ProjectionCursor{}, limits)
						if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(page, ProjectionPage[PendingLease]{}) {
							t.Error("corrupt price input escaped", page, e)
						}
						return nil
					})
				case "recovery-view":
					err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
						page, e := v.RequestAttemptPage(ctx, "req_000004", "", limits)
						if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(page, AuthenticatedRecoveryPage[ProjectedAttempt]{}) {
							t.Error("corrupt recovery input escaped", page, e)
						}
						return nil
					})
				case "apply", "authenticated-advance":
					before, e := ref.ReadReplayAnchor(ctx)
					if e != nil {
						t.Fatal(e)
					}
					root, e := os.ReadFile(filepath.Join(directory, "active.root"))
					if e != nil {
						t.Fatal(e)
					}
					// The native fact really commits. Failed derivation must preserve it
					// and leave every derived row and watermark in its previous state.
					record, _, e := native.AppendRecordWithReceipt(ctx, projectionTestLifecycle(4, true)[3])
					if e != nil {
						t.Fatal(e)
					}
					if auth {
						_, err = p.AdvanceOneFromNative(ctx, native, ProjectionPrefix{Head: Watermark{record.Generation, record.Offset, record.Sequence}, Hash: record.Hash})
						if after, e := os.ReadFile(filepath.Join(directory, "active.root")); e != nil || !bytes.Equal(after, root) {
							t.Error("failed advance published a successor", after, root, e)
						}
					} else {
						err = ref.Apply(ctx, record)
						if after, e := ref.ReadReplayAnchor(ctx); e != nil || after != before {
							t.Error("failed apply advanced watermark", after, before, e)
						}
					}
					if head, hash, _ := native.ChainHead(); head.Sequence != record.Sequence || hash != record.Hash {
						t.Error("derivation failure lost the durable native fact")
					}
					if !auth {
						if e := ref.db.View(func(tx *bbolt.Tx) error {
							position, found, e := projectionGet[Watermark](tx, "event_positions", projectionIDKey(record.Event.EventID))
							if e != nil || found || position != (Watermark{}) {
								return errors.Join(ErrProjectionCorrupt, e)
							}
							return nil
						}); e != nil {
							t.Error("failed apply leaked event idempotency index", e)
						}
					}
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("canonical reservation corruption accepted", err)
				}
				if api == "apply" {
					// Even a whole batch must roll back its earlier valid record.
					anchor, _ := ref.ReadReplayAnchor(ctx)
					first := projectionTestLifecycle(9, true)[0]
					firstRecord := projectionTestRecord(anchor.Head.Sequence+1, first)
					firstRecord.Offset = anchor.Head.Offset + 100
					secondRecord := projectionTestRecord(anchor.Head.Sequence+2, projectionTestLifecycle(4, true)[3])
					secondRecord.Offset = anchor.Head.Offset + 200
					if e := ref.ApplyBatch(ctx, []Record{firstRecord, secondRecord}); !errors.Is(e, ErrProjectionCorrupt) {
						t.Fatal("corrupt second dependency committed batch", e)
					}
					if _, found, e := ref.ReadRequest(ctx, first.RequestID); found || e != nil {
						t.Fatal("earlier batch row escaped rollback", found, e)
					}
					if after, _ := ref.ReadReplayAnchor(ctx); after != anchor {
						t.Fatal("batch failure changed cut", after, anchor)
					}
				}
			})
		}
	}
}
