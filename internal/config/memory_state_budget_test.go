package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"gopkg.in/yaml.v3"
)

func TestSelectedCompletionCeilingIsSeparateFromRequestEnvelope(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.MemoryStateRequired = true
	b.TotalRAMBytes, b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 1536<<20, 512<<20, 512<<20
	b.BackgroundRAMBytes, b.IORAMBytes, b.CompletionRAMBytes = 512<<20, 256<<20, 1<<30
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("shared ceiling was incorrectly charged to one request", err)
	}
	var requestRAM int64
	for _, slot := range ledger.CompletionWorkspaceRequirements() {
		requestRAM += slot.RAM
	}
	b.CompletionRAMBytes = requestRAM - 1
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("unfunded selected terminal profile accepted")
	}
}

func TestMemoryStateEngineContractPreservesLegacyMappingAndRejectsUnfundedWorkspace(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes = 8<<30, 8<<30, 8<<30
	b.RecoveryClaims, b.MaxClaims = 8, 16
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	// The same finite RAM profile must reject legacy mappings and accept the
	// explicitly required fixed-workspace engine. No disk ceiling is reduced.
	b.TotalRAMBytes, b.BackgroundRAMBytes, b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 2<<30, 512<<20, 512<<20, 768<<20
	b.TotalDiskBytes, b.BackgroundDiskBytes, b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 96<<30, 32<<30, 32<<30, 32<<30
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("legacy engine accepted unpaid historical mappings")
	}
	b.MemoryStateRequired = true
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("explicit fixed engine with independent disk ceilings refused", err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(raw)))
	if err != nil || !reflect.DeepEqual(round.ResourceBudget, b) {
		t.Fatal("engine requirement or budgets lost in YAML", err)
	}
	for _, change := range []func(*ResourceBudget){
		func(c *ResourceBudget) { c.BackgroundRAMBytes = 128 << 20 },
		func(c *ResourceBudget) { c.RecoveryRAMBytes = 128 << 20 },
		func(c *ResourceBudget) { c.RecoveryReserveRAMBytes = 128 << 20 },
		func(c *ResourceBudget) { c.BackgroundDiskBytes = 8 << 30 },
	} {
		candidate, copied := cfg, *b
		change(&copied)
		candidate.ResourceBudget = &copied
		if err := candidate.Validate(LoadOptions{}); err == nil {
			t.Fatal("engine requirement waived actual workspace or disk funding")
		}
	}
}
