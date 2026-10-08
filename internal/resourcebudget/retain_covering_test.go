package resourcebudget

import (
	"errors"
	"testing"
)

func TestRetainCoveringValidatesManagerDimensionsAndOwnership(t *testing.T) {
	m := fixtureManager(t)
	other := fixtureManager(t)
	p, e := m.TryAcquire(Background, Cost{RAM: 100, Disk: 200})
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []struct {
		manager *Manager
		cost    Cost
		want    error
	}{{other, Cost{Disk: 200}, ErrInvalid}, {m, Cost{Disk: 201}, ErrExhausted}, {m, Cost{}, ErrInvalid}, {m, Cost{Disk: -1}, ErrInvalid}} {
		if retained, e := p.RetainCovering(bad.manager, bad.cost); !errors.Is(e, bad.want) || retained != (Permit{}) {
			t.Fatal(retained, e)
		}
	}
	before := m.Snapshot().Used
	shared, e := p.RetainCovering(m, Cost{Disk: 200})
	if e != nil || m.Snapshot().Used != before {
		t.Fatal(e, m.Snapshot())
	}
	if e = p.ReleasePart(Cost{Disk: 1}); !errors.Is(e, ErrInvalid) {
		t.Fatal("shared persistence was refunded", e)
	}
	moved, e := p.Transfer()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.RetainCovering(m, Cost{Disk: 200}); !errors.Is(e, ErrInvalid) {
		t.Fatal("revoked owner retained", e)
	}
	if e = moved.Release(); e != nil {
		t.Fatal(e)
	}
	if cost, e := shared.Cost(); e != nil || cost.Disk != 200 {
		t.Fatal(cost, e)
	}
	if e = shared.Release(); e != nil {
		t.Fatal(e)
	}
	zeroUsage(t, m)
}
