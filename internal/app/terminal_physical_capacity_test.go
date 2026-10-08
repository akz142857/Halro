package app

import (
	"errors"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalNativeTargetCapacityCannotBeStolenByAuxiliary(t *testing.T) {
	o, manager := ledgerAppendOwnerFixture(t, 1024)
	if err := o.ReservePhysicalRange(900); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); _, err := o.reserveNativeFile(125); failures <- err }()
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if !errors.Is(err, resourcebudget.ErrExhausted) {
			t.Fatal("auxiliary borrowed owed native target", err)
		}
	}
	if err := o.reserve(125); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("ordinary append borrowed target", err)
	}
	if err := o.ConsumePhysicalRange(200); err != nil {
		t.Fatal(err)
	}
	if o.consumed() != 200 || o.physicalUnused != 700 {
		t.Fatal("actual frame was not converted from same original envelope", o.consumed(), o.physicalUnused)
	}
	if err := o.ReservePhysicalRange(125); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("target expansion exceeded shared native/aux fee", err)
	}
	if err := o.RetirePhysicalRange(700); err != nil {
		t.Fatal(err)
	}
	token, err := o.reserveNativeFile(824)
	if err != nil || o.consumed() != 1024 {
		t.Fatal("joined target retirement did not restore only reusable capacity", err, o.consumed())
	}
	if err := token.RetireKnown(); err != nil {
		t.Fatal(err)
	}
	if err := o.close(); err != nil {
		t.Fatal(err)
	}
	if o.consumed() != 200 || manager.Snapshot().Used.Disk != 1024 {
		t.Fatal("Close rebated persistent original fee", manager.Snapshot())
	}
	if err := o.ReservePhysicalRange(1); !errors.Is(err, resourcebudget.ErrClosed) {
		t.Fatal("closed owner lent target", err)
	}
}
