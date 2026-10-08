//go:build linux

package nativephysical

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalPreparedObservationActualReservationsAndControls(t *testing.T) {
	f, o, key := physicalFixture(t)
	ctx := context.Background()
	w, err := Prepare(ctx, f.Name(), f, key, Position{}, o)
	if err != nil {
		t.Fatal(err)
	}
	fees := o.Manager.Snapshot().Used
	first, err := w.ObservePrepared(ctx, Position{})
	if err != nil || first.AvailableBytes != 8192 {
		t.Fatal(first, err)
	}
	r, err := w.Reserve(Position{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := w.ObservePrepared(ctx, Position{})
	if err != nil || reserved.AvailableBytes != 4096 || reserved.Controls != first.Controls || reserved.Native != first.Native {
		t.Fatal("reservation observation lost original debt", reserved, err)
	}
	for range 3 {
		got, err := w.ObservePrepared(ctx, Position{})
		if err != nil || got != reserved {
			t.Fatal("repeated observation refunded pending native", got, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.ObservePrepared(cancelled, Position{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	control := w.controls[1-int(w.receipt.Serial%2)]
	var good [4096]byte
	if _, err := control.ReadAt(good[:], 0); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad[len(bad)-1] ^= 1
	if _, err := control.WriteAt(bad[:], 0); err != nil {
		t.Fatal(err)
	}
	if err := control.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ObservePrepared(ctx, Position{}); err == nil {
		t.Fatal("inactive control tamper acquired credit")
	}
	if _, err := control.WriteAt(good[:], 0); err != nil {
		t.Fatal(err)
	}
	if err := control.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8192); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ObservePrepared(ctx, Position{}); err == nil {
		t.Fatal("unallocated future native acquired credit")
	}
	if err := resourcebudget.AllocatePhysicalRange(f, 0, 8192); err != nil {
		t.Fatal(err)
	}
	var raw [4096]byte
	copy(raw[:], "actual authenticated native frame")
	h := hmac.New(sha256.New, key)
	h.Write(raw[:4064])
	copy(raw[4064:], h.Sum(nil))
	if err := r.BeforeWrite(Position{}, 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(raw[:], 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := r.Durable(); err != nil {
		t.Fatal(err)
	}
	head := Position{Sequence: 1, Hash: sha256.Sum256(raw[:]), EOF: 4096}
	after, err := w.ObservePrepared(ctx, head)
	if err != nil || after.AvailableBytes != 4096 || after.Controls != first.Controls || after.Native.End != first.Native.End {
		t.Fatal("native consumption refunded original capacity", after, err)
	}
	if err := w.Checkpoint(ctx, head); err != nil {
		t.Fatal(err)
	}
	checkpointed, err := w.ObservePrepared(ctx, head)
	if err != nil || checkpointed.AvailableBytes != after.AvailableBytes || checkpointed.Controls != after.Controls || checkpointed.Native != after.Native {
		t.Fatal("checkpoint minted a new allocation", checkpointed, err)
	}
	if o.Manager.Snapshot().Used != fees {
		t.Fatal("observation changed actual fees")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseAfterNative(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ObservePrepared(ctx, head); !errors.Is(err, ErrOwnership) {
		t.Fatal("closed native returned credit", err)
	}
}
