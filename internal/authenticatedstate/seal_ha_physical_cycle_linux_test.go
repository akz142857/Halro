//go:build linux

package authenticatedstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/store/lock"
)

// This is a publication-owner test. Eligibility is deliberately a synthetic
// fixture; only the App tests can prove the real Member/Ordering/source freeze.
func TestHAQualifiedPhysicalSealPreservesIndependentAuthorityAtENOSPC(t *testing.T) {
	for _, role := range []SealRole{SealPrimary, SealReplica} {
		t.Run(map[SealRole]string{SealPrimary: "Primary", SealReplica: "Replica"}[role], func(t *testing.T) {
			ctx, dir := context.Background(), t.TempDir()
			requireSealPhysicalExt4(t, dir)
			selected := sealCandidateFixture()
			selected.Role, selected.Cluster, selected.Member = role, [32]byte{3}, [32]byte{4}
			selected.Term, selected.OrderingTerm, selected.OrderingIndex = 7, 7, 9
			selected.OrderingMAC = [32]byte{5}
			authority := sealEligibilityFor(selected)
			manager, fee := sealCycleBudget(t)
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			p, err := OpenSealCandidatePublisher(dir, guard, sealCycleKey(), sealHardFor(selected), manager, fee)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.Publish(ctx, selected); err != nil {
				t.Fatal(err)
			}
			if err = p.EnablePhysicalSealPreparation(ctx, selected, fee); !errors.Is(err, ErrSealIneligible) {
				t.Fatal("Standalone entrance accepted HA", err)
			}
			if err = p.EnableHAQualifiedPhysicalSealPreparation(ctx, selected, fee, authority); err != nil {
				t.Fatal(err)
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
			before := make(map[string]os.FileInfo)
			for _, name := range []string{"active.seal", "active.seal.pending", "terminal.seal.0", "terminal.seal.1"} {
				before[name], err = os.Lstat(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			// A fresh manager and original ceiling must recover the actual births.
			manager, fee = sealCycleBudget(t)
			reader, err := OpenPhysicalSealCandidateReader(ctx, dir, guard, sealCycleKey(), sealHardFor(selected), manager, fee)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = reader.Publish(ctx, selected); err == nil {
				t.Fatal("read-only observation published")
			}
			if err = reader.EnableHAQualifiedPhysicalSealPreparation(ctx, selected, fee, authority); err == nil {
				t.Fatal("read-only observation prepared physical targets")
			}
			if err = reader.Close(); err != nil {
				t.Fatal(err)
			}
			bad := authority
			bad.Positions[0].Sequence++
			used := manager.Snapshot().Used
			if wrong, err := OpenHAQualifiedPhysicalSealCandidatePublisher(ctx, dir, guard, sealCycleKey(), sealHardFor(selected), manager, fee, selected, bad); !errors.Is(err, ErrSealIneligible) || wrong != nil {
				t.Fatal("candidate replaced independently supplied positions", err)
			}
			if manager.Snapshot().Used != used {
				t.Fatal("ineligible selection retained a new fee")
			}
			if wrong, err := OpenPhysicalSealCandidatePublisher(ctx, dir, guard, sealCycleKey(), sealHardFor(selected), manager, fee, authority); !errors.Is(err, ErrSealIneligible) || wrong != nil {
				t.Fatal("Standalone reopen accepted HA", err)
			}
			for name, original := range before {
				actual, err := os.Lstat(filepath.Join(dir, name))
				if err != nil || !os.SameFile(original, actual) {
					t.Fatal("observer or rejection changed actual inode", name, err)
				}
			}
			p, err = OpenHAQualifiedPhysicalSealCandidatePublisher(ctx, dir, guard, sealCycleKey(), sealHardFor(selected), manager, fee, selected, authority)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if err = p.EnablePhysicalSealPreparation(ctx, selected, fee); !errors.Is(err, ErrSealIneligible) {
				t.Fatal("prepared HA bypassed Standalone role guard", err)
			}
			if err = p.EnableHAQualifiedPhysicalSealPreparation(ctx, selected, fee, bad); !errors.Is(err, ErrSealIneligible) {
				t.Fatal("prepared HA bypassed independent authority", err)
			}
			if err = p.EnableHAQualifiedPhysicalSealPreparation(ctx, selected, fee, authority); err != nil {
				t.Fatal(err)
			}
			filler := exhaustSealPhysicalExt4(t, dir)
			defer os.Remove(filler)
			claims := manager.Snapshot()
			selected.Revision++
			if _, err = p.Publish(ctx, selected); err != nil {
				t.Fatal("actual retained HA target at full filesystem", err)
			}
			if err = p.ValidatePhysicalSealPreparation(ctx, selected); err != nil {
				t.Fatal(err)
			}
			after := manager.Snapshot()
			if after.Used != claims.Used || after.NormalClaims != claims.NormalClaims || after.RecoveryClaims != claims.RecoveryClaims {
				t.Fatal("HA publication borrowed fresh claims under full filesystem", claims.Used, after.Used)
			}
			if fs := requireSealPhysicalExt4(t, dir); fs.Bavail != 0 {
				t.Fatal("publication released physical preparation", fs.Bavail)
			}
		})
	}
}
