package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"gopkg.in/yaml.v3"
)

func TestSelectedLegacyMetadataFileCeilingPreservesCOWAndRejectsUnfundedMapping(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.MemoryStateRequired = true
	b.MetadataStoreDiskBytes = 576 << 20
	b.TotalRAMBytes, b.BackgroundRAMBytes = 2<<30, 512<<20
	b.RecoveryRAMBytes, b.RecoveryReserveRAMBytes = 512<<20, 768<<20
	b.TotalDiskBytes, b.BackgroundDiskBytes = 96<<30, 32<<30
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 32<<30, 32<<30
	b.RecoveryClaims, b.MaxClaims = 32, 128
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("legacy full-file mapping was accepted without its RAM")
	}
	b.LegacyMetadataFileDiskBytes = 1 << 20
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if b.MetadataStoreDiskBytes != 576<<20 {
		t.Fatal("COW disk reserve changed")
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(raw)))
	if err != nil || !reflect.DeepEqual(round.ResourceBudget, b) {
		t.Fatal("separate file ceiling lost in YAML", err)
	}
	for _, change := range []func(*ResourceBudget){
		func(c *ResourceBudget) { c.MemoryStateRequired = false },
		func(c *ResourceBudget) { c.LegacyMetadataFileDiskBytes = -1 },
		func(c *ResourceBudget) { c.LegacyMetadataFileDiskBytes = c.MetadataStoreDiskBytes + 1 },
		func(c *ResourceBudget) { c.MetadataStoreDiskBytes = 0 },
	} {
		candidate, copied := cfg, *b
		change(&copied)
		candidate.ResourceBudget = &copied
		if err := candidate.Validate(LoadOptions{}); err == nil {
			t.Fatal("invalid legacy file ceiling accepted")
		}
		if err := copied.ValidateReplicationCandidates(); err == nil {
			t.Fatal("HA qualification accepted invalid legacy file ceiling")
		}
	}
}
