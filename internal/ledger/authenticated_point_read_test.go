package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func comparePoint[T any](t *testing.T, left, right func() (T, bool, error)) {
	t.Helper()
	l, lf, le := left()
	r, rf, re := right()
	if le != nil || re != nil || lf != rf || !reflect.DeepEqual(l, r) {
		t.Fatalf("point read differs: %+v/%v/%v vs %+v/%v/%v", l, lf, le, r, rf, re)
	}
}

func TestAuthenticatedLedgerTypedPointsMatchNativeReference(t *testing.T) {
	ctx := context.Background()
	path, report, records := authenticatedLedgerNativeFixture(t)
	options := authenticatedLedgerTestOptions(t)
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	r, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Discard()
	for _, record := range records {
		e := record.Event
		comparePoint(t, func() (Watermark, bool, error) { return p.ReadEventPosition(ctx, e.EventID) }, func() (Watermark, bool, error) { return r.ReadEventPosition(ctx, e.EventID) })
		if e.RequestID != "" {
			comparePoint(t, func() (ProjectedRequest, bool, error) { return p.ReadRequest(ctx, e.RequestID) }, func() (ProjectedRequest, bool, error) { return r.ReadRequest(ctx, e.RequestID) })
			comparePoint(t, func() (Record, bool, error) { return p.ReadFinalization(ctx, e.RequestID) }, func() (Record, bool, error) { return r.ReadFinalization(ctx, e.RequestID) })
			a, ae := p.ReadLegacyRequest(ctx, e.RequestID)
			b, be := r.ReadLegacyRequest(ctx, e.RequestID)
			if ae != nil || be != nil || a != b {
				t.Fatal(a, ae, b, be)
			}
		}
		if e.AttemptID != "" {
			comparePoint(t, func() (Record, bool, error) { return p.ReadLease(ctx, e.AttemptID) }, func() (Record, bool, error) { return r.ReadLease(ctx, e.AttemptID) })
			comparePoint(t, func() (SettledAttempt, bool, error) { return p.ReadSettlement(ctx, e.AttemptID) }, func() (SettledAttempt, bool, error) { return r.ReadSettlement(ctx, e.AttemptID) })
		}
		if e.WorkUnitID != "" {
			comparePoint(t, func() (domain.WorkUnit, bool, error) { return p.ReadWorkUnit(ctx, e.WorkUnitID) }, func() (domain.WorkUnit, bool, error) { return r.ReadWorkUnit(ctx, e.WorkUnitID) })
		}
		if e.RunID != "" {
			comparePoint(t, func() (domain.Run, bool, error) { return p.ReadRun(ctx, e.RunID) }, func() (domain.Run, bool, error) { return r.ReadRun(ctx, e.RunID) })
		}
		if e.Operation != "" {
			comparePoint(t, func() (GovernanceIdempotency, bool, error) {
				return p.ReadGovernanceIdempotency(ctx, e.ProjectID, e.Operation, e.IdempotencyKeyHash)
			}, func() (GovernanceIdempotency, bool, error) {
				return r.ReadGovernanceIdempotency(ctx, e.ProjectID, e.Operation, e.IdempotencyKeyHash)
			})
		}
		key := BalanceKey{ProjectID: e.ProjectID, PeriodID: e.PeriodID, TimezoneVersion: e.PeriodTimezoneVersion}
		a, ae := p.ReadBalance(ctx, key)
		b, be := r.ReadBalance(ctx, key)
		if ae != nil || be != nil || a != b {
			t.Fatal("balance", a, ae, b, be)
		}
	}
	a, ah, ae := p.ReadPosition(ctx)
	b, bh, be := r.ReadPosition(ctx)
	if ae != nil || be != nil || a != b || ah != bh {
		t.Fatal("head mismatch", ae, be)
	}
	comparePoint(t, func() (Record, bool, error) { return p.ReadLease(ctx, "missing") }, func() (Record, bool, error) { return r.ReadLease(ctx, "missing") })
	comparePoint(t, func() (ProjectedRequest, bool, error) { return p.ReadRequest(ctx, "missing") }, func() (ProjectedRequest, bool, error) { return r.ReadRequest(ctx, "missing") })
	comparePoint(t, func() (Watermark, bool, error) { return p.ReadEventPosition(ctx, "missing") }, func() (Watermark, bool, error) { return r.ReadEventPosition(ctx, "missing") })
	lease, _, err := p.ReadLease(ctx, "att_000001")
	if err != nil {
		t.Fatal(err)
	}
	*lease.Event.ReservationMicrosUSD = 999999
	again, _, err := p.ReadLease(ctx, "att_000001")
	if err != nil || *again.Event.ReservationMicrosUSD == 999999 {
		t.Fatal("returned alias", err)
	}
}

func mutatePoint(t *testing.T, p *AuthenticatedLedgerProjection, bucket string, key []byte, value any) {
	t.Helper()
	ctx := context.Background()
	a, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic signed root for semantic rejection only; this cut is not native authority.
	a.Hash[0] ^= 0xff
	k, err := authenticatedLedgerKey(bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.store.Update(ctx, authenticatedLedgerCut(p.scope, a), func(tx *authenticatedstate.Transaction) error {
		if value == nil {
			return tx.Delete(k)
		}
		raw, ok := value.([]byte)
		if !ok {
			var err error
			raw, err = json.Marshal(value)
			if err != nil {
				return err
			}
		}
		return tx.Put(k, raw)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedLedgerTypedPointSemanticCorruptionRetiresCandidate(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"lease-id", "lease-future", "negative-balance", "request-id", "final-future", "run-id", "work-unit-id", "idempotency-id", "half-event", "missing-position", "null-position", "malformed-lease", "settlement-digest", "settlement-cost"} {
		t.Run(kind, func(t *testing.T) {
			p, native, r, _ := recoveryViewFixture(t)
			var read func() error
			switch kind {
			case "lease-id", "lease-future", "malformed-lease":
				v, _, err := p.ReadLease(ctx, "att_000004")
				if err != nil {
					t.Fatal(err)
				}
				var value any = v
				if kind == "lease-id" {
					v.Event.AttemptID = "wrong"
					value = v
				}
				if kind == "lease-future" {
					v.Sequence = 999
					value = v
				}
				if kind == "malformed-lease" {
					value = []byte("{")
				}
				mutatePoint(t, p, "leases", projectionIDKey("att_000004"), value)
				read = func() error {
					v, f, e := p.ReadLease(ctx, "att_000004")
					if f || v.Sequence != 0 {
						t.Error("corrupt lease escaped")
					}
					return e
				}
			case "negative-balance":
				key := BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-02"}
				mutatePoint(t, p, "balances", projectionBalanceKey(key), Balance{ReservedMicrosUSD: -1})
				read = func() error {
					v, e := p.ReadBalance(ctx, key)
					if v != (Balance{}) {
						t.Error("corrupt balance escaped")
					}
					return e
				}
			case "request-id", "final-future":
				v, _, err := p.ReadRequest(ctx, "req_000003")
				if err != nil {
					t.Fatal(err)
				}
				if kind == "request-id" {
					v.Accepted.Event.RequestID = "wrong"
				} else {
					v.Finalized.Sequence = 999
				}
				mutatePoint(t, p, "requests", projectionIDKey("req_000003"), v)
				read = func() error {
					_, f, e := p.ReadRequest(ctx, "req_000003")
					if f {
						t.Error("corrupt request escaped")
					}
					return e
				}
			case "run-id":
				v, _, _ := p.ReadRun(ctx, "run_1")
				v.ID = "run_2"
				mutatePoint(t, p, "runs", projectionIDKey("run_1"), v)
				read = func() error {
					_, f, e := p.ReadRun(ctx, "run_1")
					if f {
						t.Error("corrupt run escaped")
					}
					return e
				}
			case "work-unit-id":
				v, _, _ := p.ReadWorkUnit(ctx, "wku_1")
				v.ID = "wku_2"
				mutatePoint(t, p, "work_units", projectionIDKey("wku_1"), v)
				read = func() error {
					_, f, e := p.ReadWorkUnit(ctx, "wku_1")
					if f {
						t.Error("corrupt unit escaped")
					}
					return e
				}
			case "idempotency-id":
				e := projectionTestAttribution()[0]
				v, _, _ := p.ReadGovernanceIdempotency(ctx, e.ProjectID, e.Operation, e.IdempotencyKeyHash)
				v.ProjectID = "wrong"
				mutatePoint(t, p, "idempotency", projectionTuple(e.ProjectID, e.Operation, e.IdempotencyKeyHash), v)
				read = func() error {
					_, f, err := p.ReadGovernanceIdempotency(ctx, e.ProjectID, e.Operation, e.IdempotencyKeyHash)
					if f {
						t.Error("corrupt idempotency escaped")
					}
					return err
				}
			case "half-event":
				e := projectionTestAttribution()[0]
				mutatePoint(t, p, "events", projectionIDKey(e.EventID), nil)
				read = func() error {
					_, f, err := p.ReadEventPosition(ctx, e.EventID)
					if f {
						t.Error("partial index escaped")
					}
					return err
				}
			case "missing-position", "null-position":
				var value any
				if kind == "null-position" {
					value = []byte("null")
				}
				mutatePoint(t, p, "meta", []byte("position"), value)
				read = func() error { _, _, err := p.ReadPosition(ctx); return err }
			case "settlement-digest", "settlement-cost":
				v, _, _ := p.ReadSettlement(ctx, "att_000002")
				if kind == "settlement-digest" {
					v.SettlementDigest = "sha256:" + strings.Repeat("0", 64)
				} else {
					v.CostMicrosUSD++
				}
				mutatePoint(t, p, "settled", projectionIDKey("att_000002"), v)
				read = func() error {
					_, f, e := p.ReadSettlement(ctx, "att_000002")
					if f {
						t.Error("corrupt settlement escaped")
					}
					return e
				}
			}
			if err := read(); !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("semantic corruption accepted", err)
			}
			if _, _, err := p.ReadLease(ctx, "missing"); !errors.Is(err, ErrProjectionUnavailable) || !errors.Is(err, ErrProjectionCorrupt) {
				t.Fatal("candidate not retired", err)
			}
			if err := p.WithRecoveryView(ctx, func(AuthenticatedRecoveryView) error { t.Error("corrupt candidate exposed recovery"); return nil }); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal(err)
			}
			a, _ := r.ReadReplayAnchor(ctx)
			if _, err := p.AdvanceFromNative(ctx, native, ProjectionPrefix{Head: a.Head, Hash: a.Hash}); !errors.Is(err, ErrProjectionUnavailable) {
				t.Fatal("advanced corrupt owner", err)
			}
			if _, partial, err := InspectReplayAuthenticated(native.path, testChainKey, nil); err != nil || partial {
				t.Fatal("native altered", err, partial)
			}
		})
	}
}

func TestAuthenticatedLedgerPointReadOwnsDecodeUntilCancelCloseOrPanic(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "panic"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _, _ := recoveryViewFixture(t)
			before := p.budget.Snapshot().Used
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						if mode != "panic" {
							done <- errors.New("unexpected panic")
						} else {
							done <- nil
						}
					}
				}()
				v, f, err := authenticatedPointRead(ctx, p, "leases", projectionIDKey("att_000004"), func(Record, ReplayAnchor) error {
					close(entered)
					<-release
					if mode == "panic" {
						panic("decode validator panic")
					}
					return nil
				})
				if mode == "panic" {
					done <- errors.New("panic swallowed")
					return
				}
				if f || v.Sequence != 0 || err == nil {
					done <- errors.New("canceled value published")
					return
				}
				done <- nil
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("read did not reach validation")
			}
			used := p.budget.Snapshot().Used.RAM
			if used < before.RAM+AuthenticatedLedgerReadRAMBytes {
				t.Fatal("decode uncharged", used, before)
			}
			var closed chan error
			if mode == "close" {
				closed = make(chan error, 1)
				go func() { closed <- p.Close() }()
				select {
				case <-p.closeContext.Done():
				case <-time.After(time.Second):
					t.Fatal("close not canceled")
				}
				select {
				case e := <-closed:
					t.Fatal("Close returned before read joined", e)
				default:
				}
			} else if mode == "cancel" {
				cancel()
			}
			if p.budget.Snapshot().Used.RAM < before.RAM+AuthenticatedLedgerReadRAMBytes {
				t.Fatal("fee retired with live decoder")
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("read not joined")
			}
			if closed != nil {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Close not joined")
				}
			} else if used := p.budget.Snapshot().Used; used != before {
				t.Fatal("read fee leak", used, before)
			}
			if mode == "cancel" {
				if _, f, e := p.ReadLease(context.Background(), "att_000004"); e != nil || !f {
					t.Fatal("cancel poisoned owner", e)
				}
			}
			if mode == "panic" {
				if _, _, e := p.ReadLease(context.Background(), "att_000004"); !errors.Is(e, ErrProjectionUnavailable) {
					t.Fatal("panic owner remains usable", e)
				}
			}
		})
	}
}

func TestAuthenticatedLedgerPointReadRefusalIsRetryableAndNotAbsence(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	held, err := p.budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 200 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, f, e := p.ReadLease(ctx, "att_000004"); f || !errors.Is(e, resourcebudget.ErrExhausted) {
		t.Fatal("quota interpreted as absence", f, e)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	_, f, e := p.ReadLease(ctx, "att_000004")
	p.mu.Unlock()
	if f || !errors.Is(e, authenticatedstate.ErrBusy) {
		t.Fatal("queued reader", f, e)
	}
	if _, f, e := p.ReadLease(ctx, "att_000004"); e != nil || !f {
		t.Fatal("retry failed", f, e)
	}
	if err := p.budget.SetPressure(resourcebudget.Unknown); err != nil {
		t.Fatal(err)
	}
	if _, f, e := p.ReadLease(ctx, "missing"); f || !errors.Is(e, resourcebudget.ErrPressure) {
		t.Fatal("pressure interpreted as absence", f, e)
	}
	if err := p.budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	if _, f, e := p.ReadLease(ctx, "att_000004"); e != nil || !f {
		t.Fatal("pressure poisoned owner", f, e)
	}
	if _, f, e := p.ReadLease(ctx, strings.Repeat("x", maxPayloadSize+1)); f || !errors.Is(e, ErrProjectionPageLimit) {
		t.Fatal("oversized identifier accepted", e)
	}
}

func TestAuthenticatedLedgerPointFailureInvalidatesExistingRecoveryPin(t *testing.T) {
	p, _, _, _ := recoveryViewFixture(t)
	ctx := context.Background()
	err := p.WithRecoveryView(ctx, func(v AuthenticatedRecoveryView) error {
		lease, found, err := p.ReadLease(ctx, "att_000004")
		if err != nil || !found {
			t.Fatal(found, err)
		}
		lease.Event.AttemptID = "wrong"
		mutatePoint(t, p, "leases", projectionIDKey("att_000004"), lease)
		if _, _, err := p.ReadLease(ctx, "att_000004"); !errors.Is(err, ErrProjectionCorrupt) {
			t.Fatal(err)
		}
		// The pinned old root still contains the correct lease; global failure
		// must reject it too, instead of trusting an apparently healthy old pin.
		if _, found, err := v.ReadLease(ctx, "att_000004"); found || !errors.Is(err, ErrProjectionUnavailable) {
			t.Fatal("old pin survived failure", found, err)
		}
		return ErrProjectionCorrupt
	})
	if !errors.Is(err, ErrProjectionCorrupt) {
		t.Fatal(err)
	}
}

func TestAuthenticatedLedgerTypedPointsPreserveRealLeaseFirstOrigin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "native.wal")
	native := openChained(t, path, nil)
	events := projectionTestLifecycle(1, false)
	// All facts are MAC authenticated, but acceptance was historically absent.
	for _, i := range []int{1, 2, 4, 3} {
		if _, err := native.Append(ctx, events[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(err, partial)
	}
	p, err := BuildAuthenticatedLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "auth"), report, authenticatedLedgerTestOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	r, err := BuildLedgerProjection(ctx, path, testChainKey, filepath.Join(t.TempDir(), "ref.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Discard()
	id := events[1].RequestID
	comparePoint(t, func() (ProjectedRequest, bool, error) { return p.ReadRequest(ctx, id) }, func() (ProjectedRequest, bool, error) { return r.ReadRequest(ctx, id) })
	comparePoint(t, func() (Record, bool, error) { return p.ReadFinalization(ctx, id) }, func() (Record, bool, error) { return r.ReadFinalization(ctx, id) })
	v, found, err := p.ReadRequest(ctx, id)
	if err != nil || !found || v.Accepted.Sequence != 0 || v.LegacyLease == nil || v.LegacyLease.Sequence != 1 {
		t.Fatal("fabricated acceptance", v, found, err)
	}
	if legacy, err := p.ReadLegacyRequest(ctx, id); err != nil || !legacy {
		t.Fatal(legacy, err)
	}
}
