//go:build linux

package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalTerminalPersistentFeeOutlivesTemporaryRAMWorkspace(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, qualified := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "qualified"}[qualified], func(t *testing.T) {
			s, o := testStore(t, 3)
			ctx := context.Background()
			before := o.Budget.Snapshot().Used
			var native PhysicalNativeReference
			var verify func(PhysicalNativeReference, int64) error
			if qualified {
				path := filepath.Join(filepath.Dir(s.dir), "actual-native")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				native, verify = receiptNativeFixture(t, path)
			}
			err := o.Budget.WithRAMWorkspace(ctx, []resourcebudget.WorkspaceRequirement{{Class: resourcebudget.Background, RAM: 4096}}, func(ctx context.Context) error {
				if qualified {
					_, err := s.EnablePhysicalTerminalQualified(ctx, sha256.Sum256([]byte("independent-selection")), native, verify)
					return err
				}
				return s.EnablePhysicalTerminal(ctx)
			})
			if err != nil {
				t.Fatal(err)
			}
			controls := int64((maxPhysicalTerminalSlots + 2) * 4096)
			if qualified {
				controls = (5*(maxPhysicalTerminalSlots+2) + 5) * 4096
			}
			after := o.Budget.Snapshot().Used
			if after.RAM != before.RAM+(32<<10)+resourcebudget.ClaimRAMBytes || after.Disk != before.Disk+controls || s.physical == nil {
				t.Fatal("persistent fee vanished with temporary workspace", before, after)
			}
			credit, err := s.physical.fee.RetainCovering(o.Budget, resourcebudget.Cost{RAM: 32 << 10, Disk: controls})
			if err != nil {
				t.Fatal("original persistent fee is no longer live", err)
			}
			if err := credit.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
