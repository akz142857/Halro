package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	bbolt "go.etcd.io/bbolt"
)

func TestProjectionLeaseIdentityAndPhysicalCutAcrossReaders(t *testing.T) {
	for _, mutation := range []string{"id", "kind", "invalid-event", "zero-sequence", "future-sequence", "zero-generation", "future-generation", "zero-offset", "future-offset"} {
		for _, api := range []string{"ordinary-point", "authenticated-point", "pricing-point", "recovery-point", "ordinary-page", "recovery-page"} {
			t.Run(mutation+"/"+api, func(t *testing.T) {
				p, native, ref, _ := recoveryViewFixture(t)
				ctx := context.Background()
				const id = "att_000004"
				row, found, err := ref.ReadLease(ctx, id)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				anchor, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "id":
					row.Event.AttemptID = "wrong"
				case "kind":
					row.Event.Kind = EventAttemptStarted
				case "invalid-event":
					row.Event.ProjectID = ""
				case "zero-sequence":
					row.Sequence = 0
				case "future-sequence":
					row.Sequence = anchor.Head.Sequence + 1
				case "zero-generation":
					row.Generation = 0
				case "future-generation":
					row.Generation = anchor.Head.Generation + 1
				case "zero-offset":
					row.Offset = 0
				case "future-offset":
					row.Generation = anchor.Head.Generation
					row.Offset = anchor.Head.Offset + 1
				}
				auth := api != "ordinary-point" && api != "ordinary-page"
				if auth {
					mutatePoint(t, p, "leases", projectionIDKey(id), row)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, "leases", projectionIDKey(id), row) }); err != nil {
					t.Fatal(err)
				}
				before := p.budget.Snapshot()
				limits := ProjectionPageLimits{Rows: 64, Bytes: 8 << 20}
				point := func(got Record, found bool, e error) error {
					if found || !reflect.DeepEqual(got, Record{}) {
						t.Error("corrupt lease escaped", got, found)
					}
					if !errors.Is(e, ErrProjectionCorrupt) {
						t.Error("lease read accepted invalid identity/cut", e)
					}
					return e
				}
				switch api {
				case "ordinary-point":
					err = point(ref.ReadLease(ctx, id))
				case "authenticated-point":
					err = point(p.ReadLease(ctx, id))
				case "pricing-point":
					err = p.WithFreshPricingView(ctx, func(v AuthenticatedPricingView) error { point(v.ReadLease(ctx, id)); return nil })
				case "recovery-point":
					err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error { point(v.ReadLease(ctx, id)); return nil })
				case "ordinary-page":
					page, e := ref.RequestAttemptPage(ctx, "req_000004", ProjectionCursor{}, limits)
					err = e
					if !reflect.DeepEqual(page, ProjectionPage[ProjectedAttempt]{}) {
						t.Error("bad lease page escaped", page)
					}
				case "recovery-page":
					err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
						page, e := v.RequestAttemptPage(ctx, "req_000004", "", limits)
						if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(page, AuthenticatedRecoveryPage[ProjectedAttempt]{}) {
							t.Error("bad recovery lease page escaped", page, e)
						}
						return nil
					})
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("invalid lease/callback accepted", err)
				}
				if after := p.budget.Snapshot(); after.Used != before.Used || after.NormalClaims != before.NormalClaims {
					t.Error("reader changed persistent fees or retained pin", before, after)
				}
				if head, hash, _ := native.ChainHead(); head != anchor.Head || hash != anchor.Hash {
					t.Error("read failure mutated native facts")
				}
			})
		}
	}
}

func TestProjectionCorruptRequestDependencyCannotAdvance(t *testing.T) {
	for _, mutation := range []string{"accepted-generation", "final-offset", "final-project"} {
		for _, auth := range []bool{false, true} {
			t.Run(mutation+map[bool]string{false: "/ordinary", true: "/authenticated"}[auth], func(t *testing.T) {
				p, native, ref, directory := recoveryViewFixture(t)
				ctx := context.Background()
				row, found, err := ref.ReadRequest(ctx, "req_000003")
				if err != nil || !found || row.Finalized == nil {
					t.Fatal(found, err)
				}
				before, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "accepted-generation":
					row.Accepted.Generation = before.Head.Generation + 1
				case "final-offset":
					row.Finalized.Offset = before.Head.Offset + 1
				case "final-project":
					row.Finalized.Event.ProjectID = "wrong"
				}
				if auth {
					mutatePoint(t, p, "requests", projectionIDKey("req_000003"), row)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, "requests", projectionIDKey("req_000003"), row) }); err != nil {
					t.Fatal(err)
				}
				root, err := os.ReadFile(filepath.Join(directory, "active.root"))
				if err != nil {
					t.Fatal(err)
				}
				// The original pending lease has a real, durably committed late settlement.
				record, _, err := native.AppendRecordWithReceipt(ctx, projectionTestLifecycle(3, true)[3])
				if err != nil {
					t.Fatal(err)
				}
				if auth {
					_, err = p.AdvanceOneFromNative(ctx, native, ProjectionPrefix{Head: Watermark{record.Generation, record.Offset, record.Sequence}, Hash: record.Hash})
					if after, e := os.ReadFile(filepath.Join(directory, "active.root")); e != nil || !bytes.Equal(after, root) {
						t.Error("bad original request published a root", e)
					}
				} else {
					err = ref.Apply(ctx, record)
					if after, e := ref.ReadReplayAnchor(ctx); e != nil || after != before {
						t.Error("bad original request advanced cut", after, e)
					}
					if _, found, e := ref.ReadSettlement(ctx, record.Event.AttemptID); found || e != nil {
						t.Error("failed request hydration published settlement", found, e)
					}
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("corrupt request dependency accepted", err)
				}
				if head, hash, _ := native.ChainHead(); head.Sequence != record.Sequence || hash != record.Hash {
					t.Error("lost durable late settlement")
				}
			})
		}
	}
}

func TestProjectionRequestReadersPreserveNativeLegacyArrivalShapes(t *testing.T) {
	for _, shape := range []struct {
		name  string
		order []int
	}{
		{"final-only", []int{4}}, {"final-accept", []int{4, 0}},
		{"final-lease", []int{4, 1}}, {"lease-final", []int{1, 4}},
		{"lease-accept-final", []int{1, 0, 4}}, {"final-lease-accept", []int{4, 1, 0}},
		{"final-accept-lease", []int{4, 0, 1}}, {"lease-final-accept", []int{1, 4, 0}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "native.wal")
			native := openChained(t, path, nil)
			t.Cleanup(func() {
				if err := native.Close(); err != nil {
					t.Error(err)
				}
			})
			events := projectionTestLifecycle(1, false)
			// Genuine legacy requests can have attempt-specific period ownership.
			events[4].PeriodID = "2026-11-01"
			var expectedFinal, expectedLease, expectedAcceptance Record
			for _, i := range shape.order {
				record, _, err := native.AppendRecordWithReceipt(ctx, events[i])
				if err != nil {
					t.Fatal(err)
				}
				switch i {
				case 0:
					expectedAcceptance = record
				case 1:
					expectedLease = record
				case 4:
					expectedFinal = record
				}
			}
			if err := native.Close(); err != nil {
				t.Fatal(err)
			}
			report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
			if err != nil || partial {
				t.Fatal(partial, err)
			}
			options := authenticatedLedgerTestOptions(t)
			p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, options)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ordinary.db"), report)
			if err != nil {
				p.Discard()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Discard(); err != nil {
					t.Error(err)
				}
				if err := ref.Discard(); err != nil {
					t.Error(err)
				}
				if used := options.Budget.Snapshot().Used; used.RAM != 0 || used.Disk != 0 {
					t.Error("legacy fixture leaked fees", used)
				}
			})
			id := events[0].RequestID
			comparePoint(t, func() (ProjectedRequest, bool, error) { return p.ReadRequest(ctx, id) }, func() (ProjectedRequest, bool, error) { return ref.ReadRequest(ctx, id) })
			comparePoint(t, func() (Record, bool, error) { return p.ReadFinalization(ctx, id) }, func() (Record, bool, error) { return ref.ReadFinalization(ctx, id) })
			comparePoint(t, func() (Record, bool, error) { return p.ReadLease(ctx, events[1].AttemptID) }, func() (Record, bool, error) { return ref.ReadLease(ctx, events[1].AttemptID) })
			row, found, err := p.ReadRequest(ctx, id)
			if err != nil || !found || !row.Legacy() || !reflect.DeepEqual(row.Accepted, expectedAcceptance) || row.Finalized == nil || !reflect.DeepEqual(*row.Finalized, expectedFinal) {
				t.Fatal("legacy facts were rewritten or fabricated", shape.name, row, found, err)
			}
			if expectedLease.Sequence != 0 && (row.LegacyLease == nil || !reflect.DeepEqual(*row.LegacyLease, expectedLease)) {
				t.Fatal("lost original legacy lease")
			}
		})
	}
}

func TestProjectionRequestOriginalAndFinalCutsAcrossReaders(t *testing.T) {
	for _, mutation := range []string{"id", "invalid-event", "accepted-sequence", "accepted-generation", "accepted-offset", "final-generation", "final-offset", "final-project", "final-period", "final-attribution"} {
		for _, api := range []string{"ordinary-point", "authenticated-point", "ordinary-final", "authenticated-final", "recovery-point"} {
			t.Run(mutation+"/"+api, func(t *testing.T) {
				p, _, ref, _ := recoveryViewFixture(t)
				ctx := context.Background()
				const id = "req_000003"
				row, found, err := ref.ReadRequest(ctx, id)
				if err != nil || !found || row.Finalized == nil {
					t.Fatal(found, err)
				}
				anchor, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "id":
					row.Accepted.Event.RequestID = "wrong"
				case "invalid-event":
					row.Accepted.Event.ProjectID = ""
				case "accepted-sequence":
					row.Accepted.Sequence = 0
				case "accepted-generation":
					row.Accepted.Generation = anchor.Head.Generation + 1
				case "accepted-offset":
					row.Accepted.Offset = anchor.Head.Offset + 1
				case "final-generation":
					row.Finalized.Generation = anchor.Head.Generation + 1
				case "final-offset":
					row.Finalized.Offset = anchor.Head.Offset + 1
				case "final-project":
					row.Finalized.Event.ProjectID = "wrong"
				case "final-period":
					row.Finalized.Event.PeriodTimezoneVersion++
				case "final-attribution":
					row.Finalized.Event.WorkUnitID = "wku_wrong"
					row.Finalized.Event.RunID = "run_wrong"
				}
				if api == "ordinary-point" || api == "ordinary-final" {
					if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, "requests", projectionIDKey(id), row) }); err != nil {
						t.Fatal(err)
					}
				} else {
					mutatePoint(t, p, "requests", projectionIDKey(id), row)
				}
				request := func(got ProjectedRequest, found bool, e error) error {
					if found || !reflect.DeepEqual(got, ProjectedRequest{}) {
						t.Error("corrupt original/final request escaped", found)
					}
					if !errors.Is(e, ErrProjectionCorrupt) {
						t.Error("bad request returned success", e)
					}
					return e
				}
				final := func(got Record, found bool, e error) error {
					if found || !reflect.DeepEqual(got, Record{}) {
						t.Error("corrupt finalization credited terminal state", found)
					}
					return e
				}
				switch api {
				case "ordinary-point":
					err = request(ref.ReadRequest(ctx, id))
				case "authenticated-point":
					err = request(p.ReadRequest(ctx, id))
				case "ordinary-final":
					err = final(ref.ReadFinalization(ctx, id))
				case "authenticated-final":
					err = final(p.ReadFinalization(ctx, id))
				case "recovery-point":
					err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error { request(v.ReadRequest(ctx, id)); return nil })
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("bad request/callback accepted", err)
				}
			})
		}
	}
}

func TestProjectionUnfinalizedRequestPageRejectsFutureOriginal(t *testing.T) {
	for _, mutation := range []string{"sequence", "generation", "offset"} {
		for _, auth := range []bool{false, true} {
			t.Run(mutation+map[bool]string{false: "/ordinary", true: "/authenticated"}[auth], func(t *testing.T) {
				p, _, ref, _ := recoveryViewFixture(t)
				ctx := context.Background()
				const id = "req_000004"
				row, found, err := ref.ReadRequest(ctx, id)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				head, err := ref.ReadReplayAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "sequence":
					row.Accepted.Sequence = head.Head.Sequence + 1
				case "generation":
					row.Accepted.Generation = head.Head.Generation + 1
				case "offset":
					row.Accepted.Offset = head.Head.Offset + 1
				}
				if auth {
					mutatePoint(t, p, "requests", projectionIDKey(id), row)
				} else if err := ref.db.Update(func(tx *bbolt.Tx) error { return projectionPut(tx, "requests", projectionIDKey(id), row) }); err != nil {
					t.Fatal(err)
				}
				limits := ProjectionPageLimits{Rows: 64, Bytes: 8 << 20}
				if auth {
					err = p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
						page, e := v.UnfinalizedRequestPage(ctx, "", limits)
						if !errors.Is(e, ErrProjectionCorrupt) || !reflect.DeepEqual(page, AuthenticatedRecoveryPage[ProjectedRequest]{}) {
							t.Error("bad original page or prefix escaped", page, e)
						}
						return nil
					})
				} else {
					page, e := ref.UnfinalizedRequestPage(ctx, ProjectionCursor{}, limits)
					err = e
					if !reflect.DeepEqual(page, ProjectionPage[ProjectedRequest]{}) {
						t.Error("bad original page or prefix escaped", page)
					}
				}
				if !errors.Is(err, ErrProjectionCorrupt) {
					t.Fatal("future original became clean page termination", err)
				}
			})
		}
	}
}
