package authenticatedstate

import "testing"

func TestPhysicalRepackHeadroomUsesOriginalDemandAndExactCeiling(t *testing.T) {
	for _, tc := range []struct {
		name             string
		extent           uint64
		end, count, debt int64
		windows          int
		want             bool
	}{
		{"exact-vector-fits", 16, 48, 2, 0, 0, false},
		{"next-vector-short", 17, 49, 2, 0, 0, true},
		{"unknown-demand", 17, 49, 0, 0, 0, false},
		{"live-debt", 17, 49, 2, 16, 0, false},
		{"unjoined-window", 17, 49, 2, 0, 1, false},
		{"invalid-tail", 17, 65, 2, 0, 0, false},
		{"missing-tail", 17, 16, 2, 0, 0, false},
		{"unbounded-demand", 17, 49, maxPhysicalTerminalSlots + 1, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Store{ceiling: 64, appendLimit: 16, physicalExtent: tc.extent, physical: &physicalStoreOwner{allocatedEnd: tc.end, maxObservedTerminalCount: tc.count, debt: tc.debt, windows: tc.windows}}
			if got := s.physicalHeadroomExhaustedLocked(); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
