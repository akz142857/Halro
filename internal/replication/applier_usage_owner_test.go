package replication

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func TestReplicaDiskOwnerUsageDerivativeRollAndConsumerRetry(t *testing.T) {
	p, disk, _, receiver, journal, _ := rolledReplicaFixture(t, true)
	derivative, err := usage.NewLedgerDerivative(disk, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	p.ledgerDerived = []func(ledger.Record) error{derivative.Apply, func(r ledger.Record) error {
		if r.Sequence == 2 && !failed {
			failed = true
			return errors.New("next derivative failed after Usage")
		}
		return nil
	}}
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if applied, err := applier.ApplyConfirmed(ctx); err == nil || applied != 0 {
		t.Fatal("partial derivatives published", applied, err)
	}
	head, _, err := disk.ReadPosition(ctx)
	if err != nil || head.Sequence != 1 || head.Generation != 2 {
		t.Fatal(head, err)
	}
	if derivative.Aggregate().Watermark().Sequence != 2 {
		t.Fatal("fixture did not interrupt derivative-before-owner")
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
		t.Fatal(applied, err)
	}
	if derivative.Aggregate().Watermark().Sequence != 3 || derivative.Aggregate().Metrics().ActiveRequests != 3 {
		t.Fatal("retry duplicated Usage", derivative.Aggregate().Snapshot())
	}
	before := derivative.Aggregate().Snapshot()
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
		t.Fatal(applied, err)
	}
	if !reflect.DeepEqual(before, derivative.Aggregate().Snapshot()) {
		t.Fatal("applied retry changed Usage")
	}
	cold, err := usage.NewLedgerDerivative(disk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.ledgerLog.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, cold.Apply); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, cold.Aggregate().Snapshot()) {
		t.Fatal("cold/live Usage differs across physical rolls")
	}
}

func TestReplicaDiskUsageProjectionRollAndConsumerRetry(t *testing.T) {
	p, ledgerOwner, _, receiver, journal, _ := rolledReplicaFixture(t, true)
	derivative, disk, err := usage.NewDiskLedgerDerivative(ledgerOwner, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { derivative.Close(); _ = disk.Close() })
	failed := false
	p.ledgerDerived = []func(ledger.Record) error{derivative.Apply, func(r ledger.Record) error {
		if r.Sequence == 2 && !failed {
			failed = true
			return errors.New("next derivative failed after disk Usage commit")
		}
		return nil
	}}
	applier, err := NewReplicaApplier(receiver, journal, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if applied, err := applier.ApplyConfirmed(ctx); err == nil || applied != 0 {
		t.Fatal("partial disk derivative published", applied, err)
	}
	head, _, err := ledgerOwner.ReadPosition(ctx)
	if err != nil || head.Generation != 2 || head.Sequence != 1 {
		t.Fatal(head, err)
	}
	s, err := disk.ReadStats(ctx)
	if err != nil || s.Watermark.Sequence != 2 || s.Metrics.ActiveRequests != 2 {
		t.Fatal("fixture did not commit Usage before failure", s, err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
		t.Fatal(applied, err)
	}
	s, err = disk.ReadStats(ctx)
	if err != nil || s.Watermark.Sequence != 3 || s.Metrics.ActiveRequests != 3 {
		t.Fatal("retry duplicated disk Usage", s, err)
	}
	if applied, err := applier.ApplyConfirmed(ctx); err != nil || applied != 6 {
		t.Fatal(applied, err)
	}
	after, err := disk.ReadStats(ctx)
	if err != nil || s != after {
		t.Fatal("confirmed retry changed disk Usage", s, after, err)
	}
	cold, coldDisk, err := usage.NewDiskLedgerDerivative(ledgerOwner, filepath.Join(t.TempDir(), "cold-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cold.Close(); _ = coldDisk.Close() })
	if _, err := p.ledgerLog.ReplayAuthenticated(ctx, ledger.ReplayAnchor{}, cold.Apply); err != nil {
		t.Fatal(err)
	}
	coldStats, err := coldDisk.ReadStats(ctx)
	if err != nil || coldStats != s {
		t.Fatal("cold/live disk Usage differs across physical rolls", coldStats, s, err)
	}
}
