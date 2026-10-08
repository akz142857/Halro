//go:build linux

package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestUsageHAPhysicalEntryRefusesActualStandaloneOwnerBeforeAllocation(t *testing.T) {
	f := newUsagePhysicalFixture(t)
	ctx := context.Background()
	before := f.manager.Snapshot().Used
	original := f.p.physical
	raw, _ := json.Marshal(f.seal)
	if err := f.p.EnableHAPhysicalCatchup(ctx, f.native, f.owner, f.lease, sha256.Sum256(raw)); !errors.Is(err, authenticatedstate.ErrRecovery) {
		t.Fatal("Standalone constructor granted HA preparation", err)
	}
	if f.manager.Snapshot().Used != before || f.p.physical != original {
		t.Fatal("role rejection acquired physical owner or fees")
	}
}
