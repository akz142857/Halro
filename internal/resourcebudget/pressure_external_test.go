package resourcebudget_test

import (
	"errors"
	rb "github.com/akz142857/Halro/internal/resourcebudget"
	"testing"
	"time"
)

func TestDecisionCannotBeRewrappedWithHealthyOrFreshFields(t *testing.T) {
	p := rb.PressurePolicy{RuntimeEnter: 100, RuntimeExit: 80, CgroupEnter: 200, CgroupExit: 160, HeadroomEnter: 20, HeadroomExit: 40, DiskEnter: 100, DiskExit: 200, InodesEnter: 10, InodesExit: 20, MaxAge: time.Second, Cooldown: time.Millisecond}
	c, err := rb.NewController(p)
	if err != nil {
		t.Fatal(err)
	}
	l := rb.Limits{Total: rb.Cost{RAM: 4096}, MaxClaims: 2, MaxOwners: 1, MaxPressureAge: time.Second}
	l.Classes[rb.Control] = rb.Cost{RAM: 2048}
	m, err := rb.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.BindController(c); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	d := c.Observe(rb.Sample{At: at, Issues: rb.DiskUnavailable}, at)
	if d.State() != rb.Unknown || d.Issues()&rb.DiskUnavailable == 0 || d.ObservedAt() != at || d.ValidUntil() != at.Add(time.Second) {
		t.Fatal(d)
	}
	// A copied decision retains its original payload; a new envelope has no
	// authority. There are no exported writable fields for changing either.
	view := struct {
		State      rb.Pressure
		ObservedAt time.Time
	}{d.State(), d.ObservedAt()}
	view.State = rb.Healthy
	view.ObservedAt = time.Now()
	if err = m.ApplyPressure(rb.Decision{}); !errors.Is(err, rb.ErrInvalid) {
		t.Fatal(err)
	}
	if err = m.ApplyPressure(d); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Pressure != rb.Unknown {
		t.Fatal("view mutation opened admission")
	}
}
