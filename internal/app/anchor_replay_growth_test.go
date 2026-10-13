package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"gopkg.in/yaml.v3"
)

// Board §51 criterion 1: the native records Usage catch-up re-verifies per
// request stay flat as history grows, instead of replaying everything since
// startup on every settlement.
func TestSelectedAnchorVerificationReplayDoesNotGrowWithHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, configPath, source := selectedInitFixture(t)
	// Room for the whole run between checkpoints: this test observes
	// verification, not pack reclamation.
	limits := *cfg.ResourceBudget
	limits.LedgerProjectionDiskBytes, limits.UsageProjectionDiskBytes = 256<<20, 256<<20
	limits.PersistentInventoryDiskBytes = limits.MinimumPersistentInventoryDiskBytes() + 512<<20
	cfg.ResourceBudget = &limits
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if initialized, err := initializeSelected(ctx, cfg, configPath, source, nil); err != nil || !initialized {
		t.Fatal(initialized, err)
	}
	r := openSelectedInitRuntime(t, ctx, cfg, source)
	defer r.Close()
	project := domain.Project{ID: "prj_anchor_growth", Name: "anchor growth", Enabled: true}
	if _, err := r.store.PutProject(ctx, project, 0, nil); err != nil {
		t.Fatal(err)
	}
	native := r.selectedState.generation.Owners.NativeLedger
	const batch, batches = 40, 6
	var increments []int64
	for b := range batches {
		before := native.VerifiedAnchorReplays()
		for i := range batch {
			if rejected, err := selectedCapacityRequest(ctx, r, project.ID, fmt.Sprintf("req_anchor_growth_%d_%d", b, i)); err != nil || rejected {
				t.Fatal("request", b, i, rejected, err)
			}
		}
		// Let the collector settle this batch before reading the counter.
		if err := r.usageCollector.CatchUp(ctx); err != nil {
			t.Fatal(err)
		}
		increments = append(increments, native.VerifiedAnchorReplays()-before)
	}
	t.Log("records replayed per batch", increments)
	first, last := increments[1], increments[len(increments)-1]
	if last > 2*first+int64(4*batch) {
		t.Fatal("anchor verification replay grows with history", increments)
	}
}
