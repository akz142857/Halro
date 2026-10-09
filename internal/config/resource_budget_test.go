package config

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/accountinglimits"
	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/metadatalimits"
	"github.com/akz142857/Halro/internal/pricinglimits"
	"github.com/akz142857/Halro/internal/replicationlimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
	"gopkg.in/yaml.v3"
)

func TestResourceBudgetPersistentInventoryHasIndependentCeilingAndStartupWorkspace(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.PersistentInventoryDiskBytes = 100
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryRAMBytes += diskinventory.WorkspaceRAMBytes
	b.RecoveryReserveRAMBytes += diskinventory.WorkspaceRAMBytes + resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += diskinventory.WorkspaceRAMBytes + 2*resourcebudget.ClaimRAMBytes
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(encoded)))
	if err != nil || !reflect.DeepEqual(round.ResourceBudget, b) {
		t.Fatalf("inventory ceiling YAML roundtrip %v", err)
	}
	// The inventory observes ordinary files; its ceiling must not acquire a
	// bbolt mapping estimate. Background RAM is identical to the old fixture.
	if b.BackgroundRAMBytes != 32<<20 {
		t.Fatal("inventory treated as mapped projection")
	}
	recoveryNeed := b.GovernanceLogRAMBytes + b.AuditLogRAMBytes + b.PricingLifetimeRAMBytes + b.PricingIORAMBytes + b.AccountingRecoveryRAMBytes + diskinventory.WorkspaceRAMBytes
	for name, change := range map[string]func(*ResourceBudget){
		"negative":               func(v *ResourceBudget) { v.PersistentInventoryDiskBytes = -1 },
		"unbounded":              func(v *ResourceBudget) { v.PersistentInventoryDiskBytes = (1 << 50) + 1 },
		"missing-recovery-claim": func(v *ResourceBudget) { v.RecoveryClaims-- },
		"missing-normal-claim":   func(v *ResourceBudget) { v.MaxClaims = v.RecoveryClaims + 4; v.CompletionDiskBytes = 1 },
		"missing-background":     func(v *ResourceBudget) { v.BackgroundDiskBytes = 99 },
		"missing-recovery-disk":  func(v *ResourceBudget) { v.RecoveryDiskBytes = 99 },
		"missing-reserve-disk":   func(v *ResourceBudget) { v.RecoveryReserveDiskBytes = 99 },
		"missing-workspace":      func(v *ResourceBudget) { v.RecoveryRAMBytes = recoveryNeed - 1 },
		"missing-workspace-reserve": func(v *ResourceBudget) {
			v.RecoveryRAMBytes = recoveryNeed
			v.RecoveryReserveRAMBytes = recoveryNeed + int64(v.RecoveryClaims)*resourcebudget.ClaimRAMBytes - 1
		},
		"separate-growth-ceiling": func(v *ResourceBudget) {
			v.GovernanceLogDiskBytes = 1
			v.RecoveryClaims++
			v.MaxClaims += 2
			v.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
			v.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
			v.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
		},
	} {
		t.Run(name, func(t *testing.T) {
			copy := *b
			change(&copy)
			check := cfg
			check.ResourceBudget = &copy
			if err := check.Validate(LoadOptions{}); err == nil {
				t.Fatal("inventory accepted without independent complete capacity")
			}
		})
	}
}

func TestResourceBudgetPersistentInventoryHARequiresAnotherClaimWithoutMappingRAM(t *testing.T) {
	n, q, r := replicationlimits.MinCandidateRAMBytes, replicationlimits.MinQueueRAMBytes, replicationlimits.MinReadRAMBytes
	d, g, au := replicationlimits.MinNativeReaderRAMBytes, governancelimits.MinCacheRAMBytes, auditlimits.MinCacheRAMBytes
	p, io, a := pricinglimits.MinLifetimeRAMBytes, pricinglimits.MinIORAMBytes, accountinglimits.MinRecoveryRAMBytes
	ram := 3*(n+q+r) + d + g + au + p + io + a
	b := ResourceBudget{AccountingRecoveryRAMBytes: a, ReplicationCandidateRAMBytes: n, ReplicationQueueRAMBytes: q, ReplicationReadRAMBytes: r, ReplicationReaderRAMBytes: d, GovernanceLogRAMBytes: g, AuditLogRAMBytes: au, PricingLifetimeRAMBytes: p, PricingIORAMBytes: io, RecoveryClaims: 12, RecoveryRAMBytes: ram, RecoveryReserveRAMBytes: ram + 12*resourcebudget.ClaimRAMBytes}
	b.PersistentInventoryDiskBytes = 1 << 30
	if err := b.ValidateReplicationCandidates(); err == nil {
		t.Fatal("HA inventory omitted dedicated claim/workspace")
	}
	b.RecoveryClaims++
	b.RecoveryRAMBytes += diskinventory.WorkspaceRAMBytes
	b.RecoveryReserveRAMBytes += diskinventory.WorkspaceRAMBytes + resourcebudget.ClaimRAMBytes
	if err := b.ValidateReplicationCandidates(); err != nil {
		t.Fatalf("ordinary inventory wrongly charged mapping RAM %v", err)
	}
	b.RecoveryRAMBytes--
	if err := b.ValidateReplicationCandidates(); err == nil {
		t.Fatal("HA inventory ignored exact workspace capacity")
	}
}

func TestResourceBudgetLedgerAppendCapacityAddsToExistingInventory(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.PersistentInventoryDiskBytes = 64
	b.LedgerAppendDiskBytes = 36
	b.RecoveryClaims += 2
	b.MaxClaims += 4
	b.ControlRAMBytes += 4 * resourcebudget.ClaimRAMBytes
	b.RecoveryRAMBytes += diskinventory.WorkspaceRAMBytes
	b.RecoveryReserveRAMBytes += diskinventory.WorkspaceRAMBytes + 2*resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += diskinventory.WorkspaceRAMBytes + 4*resourcebudget.ClaimRAMBytes
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("whole existing+growth aggregate rejected", err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(encoded)))
	if err != nil || !reflect.DeepEqual(round.ResourceBudget, b) {
		t.Fatal("native append ceiling YAML roundtrip lost", err)
	}
	for name, change := range map[string]func(*ResourceBudget){
		"existing-tree-unfunded":          func(v *ResourceBudget) { v.PersistentInventoryDiskBytes = 0 },
		"negative":                        func(v *ResourceBudget) { v.LedgerAppendDiskBytes = -1 },
		"unbounded":                       func(v *ResourceBudget) { v.LedgerAppendDiskBytes = (1 << 50) + 1 },
		"additional-growth-over-capacity": func(v *ResourceBudget) { v.LedgerAppendDiskBytes = 37 },
		"missing-background":              func(v *ResourceBudget) { v.BackgroundDiskBytes = 99 },
		"missing-normal":                  func(v *ResourceBudget) { v.TotalDiskBytes = v.RecoveryReserveDiskBytes + 99 },
		"missing-growth-recovery-claim":   func(v *ResourceBudget) { v.RecoveryClaims-- },
		"missing-growth-normal-claim":     func(v *ResourceBudget) { v.MaxClaims = v.RecoveryClaims + 4 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *b
			change(&bad)
			check := cfg
			check.ResourceBudget = &bad
			if err := check.Validate(LoadOptions{}); err == nil {
				t.Fatal("native append growth borrowed stock/recovery/shared capacity")
			}
		})
	}
}

func TestResourceBudgetMetadataFileAlsoReservesMappingAndConcurrentWork(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.MetadataStoreDiskBytes = 1 << 20
	b.TotalDiskBytes, b.BackgroundDiskBytes = 64<<20, 16<<20
	b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 16<<20, 16<<20
	b.RecoveryClaims++
	b.MaxClaims += 2
	b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	ram, _ := metadatalimits.MappingRAMBytes(b.MetadataStoreDiskBytes)
	b.TotalRAMBytes += 2 * ram
	b.BackgroundRAMBytes += ram
	b.RecoveryRAMBytes += ram
	b.RecoveryReserveRAMBytes += ram
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(encoded)))
	if err != nil || !reflect.DeepEqual(round.ResourceBudget, b) {
		t.Fatal("Metadata file limit did not survive YAML roundtrip", err)
	}
	for name, change := range map[string]func(*ResourceBudget){
		"mapping-overflow":                   func(v *ResourceBudget) { v.MetadataStoreDiskBytes = math.MaxInt64 },
		"missing-recovery-mapping":           func(v *ResourceBudget) { v.RecoveryRAMBytes -= ram },
		"missing-background-mapping":         func(v *ResourceBudget) { v.BackgroundRAMBytes = ram - 1 },
		"no-deferred-capacity-after-mapping": func(v *ResourceBudget) { v.BackgroundRAMBytes = ram },
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			copy := *b
			change(&copy)
			bad.ResourceBudget = &copy
			if err := bad.Validate(LoadOptions{}); err == nil {
				t.Fatal("configured file accepted without mapping/work capacity")
			}
		})
	}
}

const ramBudgetConfigFixture = `
resource_budget:
  total_ram_bytes: 236980224
  total_disk_bytes: 1000
  background_disk_bytes: 800
  completion_disk_bytes: 50
  recovery_disk_bytes: 100
  recovery_reserve_disk_bytes: 100
  active_ram_bytes: 33554432
  background_ram_bytes: 33554432
  control_ram_bytes: 11264
  recovery_reserve_ram_bytes: 178258944
  recovery_ram_bytes: 178254848
  max_claims: 11
  recovery_claims: 5
  max_owners: 4
  request_ram_bytes: 16777216
  deferred_ram_bytes: 8388608
  audit_log_ram_bytes: 2097152
  governance_log_ram_bytes: 2097152
  pricing_lifetime_ram_bytes: 1048576
  accounting_recovery_ram_bytes: 134217728
  pricing_io_ram_bytes: 33554432
  io_ram_bytes: 33554432
`

func TestResourceBudgetRequiresCalibratedSharedRAMAndPressure(t *testing.T) {
	if Default().ResourceBudget != nil {
		t.Fatal("RAM budget must be explicit")
	}
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	l := cfg.ResourceBudget.Limits(cfg.ResourcePressure.MaxAge.Value())
	if l.Total.RAM != (226<<20)+2*resourcebudget.ClaimRAMBytes || l.Total.Disk != 1000 || l.Total.Unconfirmed != 0 || l.Classes[resourcebudget.Background].RAM != 32<<20 || l.Classes[resourcebudget.Background].Disk != 800 || l.Classes[resourcebudget.Completion].Disk != 50 || l.Classes[resourcebudget.Recovery].Disk != 100 || l.RecoveryReserve.Disk != 100 || l.MaxPressureAge != cfg.ResourcePressure.MaxAge.Value() {
		t.Fatal(l)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	round, err := Decode(strings.NewReader(string(b)))
	if err != nil || !reflect.DeepEqual(round.ResourceBudget, cfg.ResourceBudget) {
		t.Fatal(err, round.ResourceBudget)
	}
	manager, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal("calibrated disk limits rejected", err)
	}
	if err = manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	background, err := manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 800})
	if err != nil {
		t.Fatal("background disk envelope rejected", err)
	}
	if _, err = manager.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 1}); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("shared disk ceiling not enforced", err)
	}
	completion, err := manager.TryAcquireWork(resourcebudget.Active, resourcebudget.Cost{RAM: 1}, resourcebudget.Cost{Disk: 50})
	if err != nil {
		t.Fatal("completion disk reserve not admitted", err)
	}
	recovery, err := manager.TryAcquireRecovery(resourcebudget.Cost{Disk: 100})
	if err != nil {
		t.Fatal("recovery disk reserve not admitted", err)
	}
	if _, err = manager.TryAcquireRecovery(resourcebudget.Cost{Disk: 1}); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("recovery reserve was borrowable", err)
	}
	if got := manager.Snapshot().Used; got != (resourcebudget.Cost{RAM: 4*resourcebudget.ClaimRAMBytes + 1, Disk: 950}) {
		t.Fatal("disk charges do not include fixed claim RAM and completion reservation", got)
	}
	_ = background.Release()
	_ = completion.Active.Release()
	_ = completion.Completion.Release()
	_ = recovery.Release()

	for _, shape := range []func(*Config){
		func(c *Config) { c.ResourcePressure = nil },
		func(c *Config) { c.ResourceBudget.GovernanceLogRAMBytes = 0 },
		func(c *Config) { c.ResourceBudget.AuditLogRAMBytes = 0 },
		func(c *Config) { c.ResourceBudget.AuditLogRAMBytes-- },
		func(c *Config) { c.ResourceBudget.AuditLogRAMBytes = math.MaxInt64 },
		func(c *Config) { c.ResourceBudget.PricingLifetimeRAMBytes = 0 },
		func(c *Config) { c.ResourceBudget.PricingLifetimeRAMBytes = math.MaxInt64 },
		func(c *Config) { c.ResourceBudget.AccountingRecoveryRAMBytes-- },
		func(c *Config) { c.ResourceBudget.AccountingRecoveryRAMBytes = math.MaxInt64 },
		func(c *Config) { c.ResourceBudget.PricingIORAMBytes-- },
		func(c *Config) { c.ResourceBudget.PricingIORAMBytes = math.MaxInt64 },
		func(c *Config) { c.ResourceBudget.IORAMBytes-- },
		func(c *Config) { c.ResourceBudget.GovernanceLogRAMBytes-- },
		func(c *Config) { c.ResourceBudget.GovernanceLogRAMBytes = math.MaxInt64 },
		func(c *Config) { c.ResourceBudget.RecoveryClaims = 0 },
		func(c *Config) { c.ResourceBudget.MaxClaims = c.ResourceBudget.RecoveryClaims + 1 },
		func(c *Config) { c.ResourceBudget.ControlRAMBytes-- },
		func(c *Config) { c.ResourceBudget.RequestRAMBytes = c.Server.MaxRequestBytes - 1 },
		func(c *Config) { c.ResourceBudget.DeferredRAMBytes = 0 },
		func(c *Config) { c.ResourceBudget.DeferredRAMBytes = c.ResourceBudget.BackgroundRAMBytes + 1 },
		func(c *Config) {
			c.ResourceBudget.TotalRAMBytes = c.ResourceBudget.RecoveryReserveRAMBytes + c.ResourceBudget.RequestRAMBytes
		},
		func(c *Config) { c.ResourceBudget.RecoveryClaims = c.ResourceBudget.MaxClaims },
		func(c *Config) { c.ResourceBudget.RecoveryRAMBytes = c.ResourceBudget.RecoveryReserveRAMBytes + 1 },
		func(c *Config) { c.ResourceBudget.TotalDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.CompletionRAMBytes = c.ResourceBudget.TotalRAMBytes },
		func(c *Config) { c.ResourceBudget.CompletionDiskBytes = c.ResourceBudget.TotalDiskBytes },
		func(c *Config) { c.ResourceBudget.BackgroundDiskBytes = c.ResourceBudget.TotalDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.RecoveryDiskBytes++ },
		func(c *Config) { c.ResourceBudget.RecoveryReserveDiskBytes = c.ResourceBudget.TotalDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.LedgerSegmentIndexDiskBytes = -1 },
		func(c *Config) {
			c.ResourceBudget.LedgerSegmentIndexDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1
		},
		func(c *Config) { c.ResourceBudget.LedgerProjectionDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.LedgerProjectionDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.UsageProjectionDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.UsageProjectionDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.UsageArchiveDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.UsageArchiveDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.GovernanceProjectionDiskBytes = -1 },
		func(c *Config) {
			c.ResourceBudget.GovernanceProjectionDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1
		},
		func(c *Config) { c.ResourceBudget.GovernanceLogDiskBytes = -1 },
		func(c *Config) {
			c.ResourceBudget.GovernanceLogDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1
		},
		func(c *Config) { c.ResourceBudget.AuditLogDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.AuditLogDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.MetadataStoreDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.MetadataStoreDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.MetadataJournalDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.MetadataJournalDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.OrderingJournalDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.OrderingJournalDiskBytes = c.ResourceBudget.BackgroundDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.BackupWorkspaceDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.BackupWorkspaceDiskBytes = (1 << 50) + 1 },
		func(c *Config) { c.ResourceBudget.ProviderObjectReferenceDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.ProviderObjectReferenceDiskBytes = 1 },
		func(c *Config) { c.ResourceBudget.ProviderObjectReferenceDiskBytes = 1 << 41 },
		func(c *Config) {
			c.ResourceBudget.ProviderObjectReferenceDiskBytes = c.ResourceBudget.IODiskBytes + 1024
		},
		func(c *Config) { c.ResourceBudget.BackupWorkspaceDiskBytes = c.ResourceBudget.RecoveryDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.RestoreWorkspaceDiskBytes = -1 },
		func(c *Config) { c.ResourceBudget.RestoreWorkspaceDiskBytes = c.ResourceBudget.RecoveryDiskBytes + 1 },
		func(c *Config) { c.ResourceBudget.UsageArchiveReadRAMBytes = -1 },
		func(c *Config) { c.ResourceBudget.UsageArchiveReadRAMBytes = math.MaxInt64 },
	} {
		c := cfg
		copy := *cfg.ResourceBudget
		c.ResourceBudget = &copy
		shape(&c)
		if err = c.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "resource_budget") {
			t.Fatal("invalid budget accepted", err, c.ResourceBudget)
		}
	}
	projectionBudget := *cfg.ResourceBudget
	projectionBudget.LedgerProjectionDiskBytes = 64
	addProjectionMappingConfigFixture(t, &projectionBudget)
	projectionBudget.RecoveryClaims = 6
	projectionCfg := cfg
	projectionCfg.ResourceBudget = &projectionBudget
	if err := projectionCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Ledger projection disk ceiling rejected", err)
	}
	segmentIndexBudget := *cfg.ResourceBudget
	segmentIndexBudget.LedgerSegmentIndexDiskBytes = 64
	segmentIndexBudget.RecoveryClaims = 6
	segmentIndexCfg := cfg
	segmentIndexCfg.ResourceBudget = &segmentIndexBudget
	if err := segmentIndexCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Ledger segment index disk ceiling rejected", err)
	}
	usageBudget := *cfg.ResourceBudget
	usageBudget.UsageProjectionDiskBytes = 64
	addProjectionMappingConfigFixture(t, &usageBudget)
	usageBudget.RecoveryClaims = 6
	usageCfg := cfg
	usageCfg.ResourceBudget = &usageBudget
	if err := usageCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Usage projection disk ceiling rejected", err)
	}
	usageArchiveBudget := *cfg.ResourceBudget
	usageArchiveBudget.UsageArchiveDiskBytes = 64
	usageArchiveBudget.IODiskBytes = 64
	usageArchiveBudget.RecoveryClaims = 7
	usageArchiveBudget.MaxClaims += 5
	usageArchiveBudget.ControlRAMBytes += 5 * resourcebudget.ClaimRAMBytes
	usageArchiveBudget.TotalRAMBytes += 5 * resourcebudget.ClaimRAMBytes
	usageArchiveBudget.RecoveryReserveRAMBytes += 2 * resourcebudget.ClaimRAMBytes
	usageArchiveCfg := cfg
	usageArchiveCfg.ResourceBudget = &usageArchiveBudget
	if err := usageArchiveCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Usage archive disk ceiling rejected", err)
	}
	missingArchiveIODisk := usageArchiveBudget
	missingArchiveIODisk.IODiskBytes = 0
	usageArchiveCfg.ResourceBudget = &missingArchiveIODisk
	if err := usageArchiveCfg.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "io_disk_bytes") {
		t.Fatal("Usage archive transient work accepted without an IO disk envelope", err)
	}
	insufficientArchiveOwners := usageArchiveBudget
	insufficientArchiveOwners.MaxClaims = insufficientArchiveOwners.RecoveryClaims + 3
	insufficientArchiveOwners.ControlRAMBytes = int64(insufficientArchiveOwners.MaxClaims) * resourcebudget.ClaimRAMBytes
	insufficientArchiveCfg := cfg
	insufficientArchiveCfg.ResourceBudget = &insufficientArchiveOwners
	if err := insufficientArchiveCfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("Usage archive owner accepted without a normal claim slot")
	}
	usageArchiveRAMBudget := *cfg.ResourceBudget
	usageArchiveRAMBudget.UsageArchiveReadRAMBytes = 1 << 18
	usageArchiveRAMBudget.RecoveryClaims = 9
	usageArchiveRAMBudget.MaxClaims = 17
	usageArchiveRAMBudget.ControlRAMBytes = int64(usageArchiveRAMBudget.MaxClaims) * resourcebudget.ClaimRAMBytes
	usageArchiveRAMBudget.TotalRAMBytes += 6 * resourcebudget.ClaimRAMBytes
	usageArchiveRAMBudget.RecoveryReserveRAMBytes += 6*resourcebudget.ClaimRAMBytes + usage.ArchiveReaderSlots*usageArchiveRAMBudget.UsageArchiveReadRAMBytes
	usageArchiveRAMBudget.RecoveryRAMBytes += usage.ArchiveReaderSlots * usageArchiveRAMBudget.UsageArchiveReadRAMBytes
	usageArchiveRAMCfg := cfg
	usageArchiveRAMCfg.ResourceBudget = &usageArchiveRAMBudget
	if err := usageArchiveRAMCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Usage archive reader RAM ceiling rejected", err)
	}
	usageArchiveRAMYAML, err := yaml.Marshal(usageArchiveRAMCfg)
	if err != nil {
		t.Fatal(err)
	}
	usageArchiveRAMRoundTrip, err := Decode(strings.NewReader(string(usageArchiveRAMYAML)))
	if err != nil || !reflect.DeepEqual(usageArchiveRAMRoundTrip.ResourceBudget, usageArchiveRAMCfg.ResourceBudget) {
		t.Fatal("Usage archive reader RAM setting did not survive YAML round trip", err, usageArchiveRAMRoundTrip.ResourceBudget)
	}
	insufficientArchiveReaderSlots := usageArchiveRAMBudget
	insufficientArchiveReaderSlots.MaxClaims = insufficientArchiveReaderSlots.RecoveryClaims + 6
	insufficientArchiveReaderSlots.ControlRAMBytes = int64(insufficientArchiveReaderSlots.MaxClaims) * resourcebudget.ClaimRAMBytes
	insufficientArchiveReaderCfg := cfg
	insufficientArchiveReaderCfg.ResourceBudget = &insufficientArchiveReaderSlots
	if err := insufficientArchiveReaderCfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("Usage archive readers accepted without enough normal claim slots")
	}
	governanceBudget := *cfg.ResourceBudget
	governanceBudget.GovernanceProjectionDiskBytes = 64
	addProjectionMappingConfigFixture(t, &governanceBudget)
	governanceBudget.RecoveryClaims = 6
	governanceCfg := cfg
	governanceCfg.ResourceBudget = &governanceBudget
	if err := governanceCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Governance projection disk ceiling rejected", err)
	}
	governanceJournalBudget := *cfg.ResourceBudget
	governanceJournalBudget.GovernanceLogDiskBytes = 64
	governanceJournalBudget.RecoveryClaims = 6
	governanceJournalCfg := cfg
	governanceJournalCfg.ResourceBudget = &governanceJournalBudget
	if err := governanceJournalCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Governance journal disk ceiling rejected", err)
	}
	auditJournalBudget := *cfg.ResourceBudget
	auditJournalBudget.AuditLogDiskBytes = 64
	auditJournalBudget.RecoveryClaims = 6
	auditJournalCfg := cfg
	auditJournalCfg.ResourceBudget = &auditJournalBudget
	if err := auditJournalCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Audit journal disk ceiling rejected", err)
	}
	metadataJournalBudget := *cfg.ResourceBudget
	metadataJournalBudget.MetadataJournalDiskBytes = 64
	metadataJournalBudget.RecoveryClaims = 6
	metadataJournalCfg := cfg
	metadataJournalCfg.ResourceBudget = &metadataJournalBudget
	if err := metadataJournalCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Metadata journal disk ceiling rejected", err)
	}
	orderingJournalBudget := *cfg.ResourceBudget
	orderingJournalBudget.OrderingJournalDiskBytes = 64
	orderingJournalBudget.RecoveryClaims = 6
	orderingJournalCfg := cfg
	orderingJournalCfg.ResourceBudget = &orderingJournalBudget
	if err := orderingJournalCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated Ordering journal disk ceiling rejected", err)
	}
	combined := *cfg.ResourceBudget
	combined.LedgerProjectionDiskBytes = 32
	combined.UsageProjectionDiskBytes = 24
	combined.UsageArchiveDiskBytes = 4
	combined.IODiskBytes = 100
	combined.GovernanceProjectionDiskBytes = 8
	addProjectionMappingConfigFixture(t, &combined)
	combined.GovernanceLogDiskBytes = 8
	combined.AuditLogDiskBytes = 8
	combined.MetadataJournalDiskBytes = 8
	combined.OrderingJournalDiskBytes = 8
	combined.RecoveryClaims = 14
	combined.MaxClaims = 31
	combined.ControlRAMBytes = int64(combined.MaxClaims) * resourcebudget.ClaimRAMBytes
	combined.TotalRAMBytes += 3 * resourcebudget.ClaimRAMBytes
	combined.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	usageCfg.ResourceBudget = &combined
	if err := usageCfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("simultaneous Ledger/Usage/Governance/Audit/Metadata/Ordering persistent ceilings rejected", err)
	}
	combined.UsageProjectionDiskBytes = 80
	usageCfg.ResourceBudget = &combined
	if err := usageCfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("combined startup projection ceilings exceeded Recovery envelope without rejection")
	}
	for _, shape := range []func(*ResourceBudget){
		func(b *ResourceBudget) { b.RecoveryDiskBytes = 63 },
		func(b *ResourceBudget) { b.RecoveryReserveDiskBytes = 63 },
		func(b *ResourceBudget) { b.RecoveryReserveDiskBytes = b.TotalDiskBytes - 50 },
		func(b *ResourceBudget) { b.RecoveryClaims = 5 },
	} {
		budget := projectionBudget
		shape(&budget)
		c := cfg
		c.ResourceBudget = &budget
		if err := c.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "resource_budget") {
			t.Fatal("unfunded Ledger projection disk ceiling accepted", err, budget)
		}
	}
	if _, err = Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture + "  disk_bytes: 1\n")); err == nil {
		t.Fatal("unknown disk budget key silently accepted")
	}
}

func TestResourceBudgetMapsAndValidatesUnconfirmedRecoveryReserve(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.TotalUnconfirmedBytes = 64 << 20
	b.IOUnconfirmedBytes = 32 << 20
	b.RecoveryUnconfirmedBytes = 16 << 20
	b.RecoveryReserveUnconfirmedBytes = 16 << 20
	b.UnconfirmedMaxEntries = 512
	b.ControlRAMBytes = 12 * resourcebudget.ClaimRAMBytes
	b.MaxClaims = 12
	b.RecoveryClaims = 7
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	limits := b.Limits(cfg.ResourcePressure.MaxAge.Value())
	if limits.Total.Unconfirmed != b.TotalUnconfirmedBytes || limits.Classes[resourcebudget.IO].Unconfirmed != b.IOUnconfirmedBytes || limits.Classes[resourcebudget.Recovery].Unconfirmed != b.RecoveryUnconfirmedBytes || limits.RecoveryReserve.Unconfirmed != b.RecoveryReserveUnconfirmedBytes {
		t.Fatal("unconfirmed ceilings were not mapped to shared manager", limits)
	}
	for name, mutate := range map[string]func(){
		"no_recovery_reserve":      func() { b.RecoveryReserveUnconfirmedBytes = 0 },
		"normal_overlaps_recovery": func() { b.IOUnconfirmedBytes = b.TotalUnconfirmedBytes },
		"class_exceeds_reserve":    func() { b.RecoveryUnconfirmedBytes = b.RecoveryReserveUnconfirmedBytes - 1 },
		"missing_entry_limit":      func() { b.UnconfirmedMaxEntries = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			original := *b
			defer func() { *b = original }()
			mutate()
			if err := cfg.Validate(LoadOptions{}); err == nil {
				t.Fatal("invalid unconfirmed budget accepted")
			}
		})
	}
}

func TestBackupWorkspaceDiskBudgetSharesRecoveryWithPersistentCeilings(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := *cfg.ResourceBudget
	b.BackupWorkspaceDiskBytes = 24
	b.RecoveryClaims = 6
	backupRAM := int64(24 + (32 << 20))
	b.TotalRAMBytes += backupRAM
	b.RecoveryRAMBytes += backupRAM
	b.RecoveryReserveRAMBytes += backupRAM
	check := cfg
	check.ResourceBudget = &b
	if err := check.Validate(LoadOptions{}); err != nil {
		t.Fatal("backup workspace ceiling did not fit its Recovery owner", err)
	}
	b.LedgerProjectionDiskBytes = 76
	b.RecoveryClaims = 7
	addProjectionMappingConfigFixture(t, &b)
	b.MaxClaims = 12
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	check.ResourceBudget = &b
	if err := check.Validate(LoadOptions{}); err != nil {
		t.Fatal("backup and persistent startup ceilings should fit at exactly the Recovery limit", err)
	}
	b.BackupWorkspaceDiskBytes = 25
	check.ResourceBudget = &b
	if err := check.Validate(LoadOptions{}); err == nil {
		t.Fatal("backup and persistent startup ceilings exceeded Recovery but were accepted")
	}
	b.BackupWorkspaceDiskBytes = 0
	b.RestoreWorkspaceDiskBytes = 24
	b.RecoveryClaims = 7
	check.ResourceBudget = &b
	if err := check.Validate(LoadOptions{}); err != nil {
		t.Fatal("restore workspace ceiling did not fit its Recovery owner", err)
	}
	b.LedgerProjectionDiskBytes = 76
	check.ResourceBudget = &b
	if err := check.Validate(LoadOptions{}); err != nil {
		t.Fatal("restore and persistent startup ceilings should fit at exactly the Recovery limit", err)
	}
	b.RestoreWorkspaceDiskBytes = 25
	check.ResourceBudget = &b
	if err := check.Validate(LoadOptions{}); err == nil {
		t.Fatal("restore and persistent startup ceilings exceeded Recovery but were accepted")
	}
}

func TestResourceBudgetCaptureEnvelopeRequiresOwnedFullPipelineCapacity(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture + "  capture_ram_bytes: 16777216\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.FailureCapture.Enabled = true
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []func(*Config){
		func(c *Config) { c.ResourceBudget.CaptureRAMBytes = 0 },
		func(c *Config) { c.ResourceBudget.CaptureRAMBytes = -1 },
		func(c *Config) { c.ResourceBudget.CaptureRAMBytes = c.ResourceBudget.RequestRAMBytes - 1 },
		func(c *Config) { c.ResourceBudget.CaptureRAMBytes = c.ResourceBudget.BackgroundRAMBytes + 1 },
		func(c *Config) {
			c.ResourceBudget.TotalRAMBytes = c.ResourceBudget.RecoveryReserveRAMBytes + c.ResourceBudget.CaptureRAMBytes
		},
		func(c *Config) { c.ResourceBudget.MaxOwners = 2 },
	} {
		copy := cfg
		budget := *cfg.ResourceBudget
		copy.ResourceBudget = &budget
		shape(&copy)
		if err := copy.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "resource_budget") {
			t.Fatal("invalid capture envelope accepted", err)
		}
	}
	budget := ResourceBudget{CaptureRAMBytes: 2 << 20, MaxOwners: 3}
	if err := budget.ValidateCapture(FailureCapture{Enabled: true, MaxBytes: 1 << 20}); err == nil {
		t.Fatal("three side limits did not constrain envelope")
	}
	budget.CaptureRAMBytes = 3 << 20
	if err := budget.ValidateCapture(FailureCapture{Enabled: true, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := budget.ValidateCapture(FailureCapture{Enabled: true, MaxBytes: math.MaxInt}); err == nil {
		t.Fatal("overflowing side limit accepted")
	}
	if err := (ResourceBudget{}).ValidateCapture(FailureCapture{Enabled: false}); err != nil {
		t.Fatal("disabled capture requires no envelope", err)
	}
}

func TestResourceBudgetReplicationCandidateCapacityRejectsUnderflowAndOverflow(t *testing.T) {
	n := replicationlimits.MinCandidateRAMBytes
	q := replicationlimits.MinQueueRAMBytes
	r := replicationlimits.MinReadRAMBytes
	d := replicationlimits.MinNativeReaderRAMBytes
	g := governancelimits.MinCacheRAMBytes
	au := auditlimits.MinCacheRAMBytes
	p := pricinglimits.MinLifetimeRAMBytes
	io := pricinglimits.MinIORAMBytes
	a := accountinglimits.MinRecoveryRAMBytes
	b := ResourceBudget{AccountingRecoveryRAMBytes: a, ReplicationCandidateRAMBytes: n, ReplicationQueueRAMBytes: q, ReplicationReadRAMBytes: r, ReplicationReaderRAMBytes: d, GovernanceLogRAMBytes: g, AuditLogRAMBytes: au, PricingLifetimeRAMBytes: p, PricingIORAMBytes: io, RecoveryClaims: 12, RecoveryRAMBytes: 3*(n+q+r) + d + g + au + p + io + a, RecoveryReserveRAMBytes: 3*(n+q+r) + d + g + au + p + io + a + 12*resourcebudget.ClaimRAMBytes}
	if err := b.ValidateReplicationCandidates(); err != nil {
		t.Fatal(err)
	}
	withProjectionDisk := b
	withProjectionDisk.LedgerProjectionDiskBytes = 1
	if err := withProjectionDisk.ValidateReplicationCandidates(); err == nil {
		t.Fatal("HA disk owner did not reserve its additional Recovery claim")
	}
	withProjectionDisk.RecoveryClaims = 13
	withProjectionDisk.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	withProjectionDisk.RecoveryRAMBytes += 1 + (32 << 20)
	withProjectionDisk.RecoveryReserveRAMBytes += 1 + (32 << 20)
	if err := withProjectionDisk.ValidateReplicationCandidates(); err != nil {
		t.Fatal("HA disk owner with a dedicated Recovery claim rejected", err)
	}
	withBothProjectionDisks := withProjectionDisk
	withBothProjectionDisks.UsageProjectionDiskBytes = 1
	if err := withBothProjectionDisks.ValidateReplicationCandidates(); err == nil {
		t.Fatal("second HA disk owner did not reserve another Recovery claim")
	}
	withBothProjectionDisks.RecoveryClaims = 14
	withBothProjectionDisks.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	withBothProjectionDisks.RecoveryRAMBytes += 1 + (32 << 20)
	withBothProjectionDisks.RecoveryReserveRAMBytes += 1 + (32 << 20)
	if err := withBothProjectionDisks.ValidateReplicationCandidates(); err != nil {
		t.Fatal("HA Ledger and Usage disk owners with dedicated claims rejected", err)
	}
	withGovernanceProjectionDisk := withBothProjectionDisks
	withGovernanceProjectionDisk.GovernanceProjectionDiskBytes = 1
	if err := withGovernanceProjectionDisk.ValidateReplicationCandidates(); err == nil {
		t.Fatal("third HA disk owner did not reserve another Recovery claim")
	}
	withGovernanceProjectionDisk.RecoveryClaims = 15
	withGovernanceProjectionDisk.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	withGovernanceProjectionDisk.RecoveryRAMBytes += 1 + (32 << 20)
	withGovernanceProjectionDisk.RecoveryReserveRAMBytes += 1 + (32 << 20)
	if err := withGovernanceProjectionDisk.ValidateReplicationCandidates(); err != nil {
		t.Fatal("HA with all three projection disk owners and dedicated claims rejected", err)
	}
	withGovernanceJournalDisk := withGovernanceProjectionDisk
	withGovernanceJournalDisk.GovernanceLogDiskBytes = 1
	if err := withGovernanceJournalDisk.ValidateReplicationCandidates(); err == nil {
		t.Fatal("Governance journal disk owner did not reserve its additional Recovery claim")
	}
	withGovernanceJournalDisk.RecoveryClaims = 16
	withGovernanceJournalDisk.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	if err := withGovernanceJournalDisk.ValidateReplicationCandidates(); err != nil {
		t.Fatal("HA Governance journal disk owner with a dedicated Recovery claim rejected", err)
	}
	withAuditJournalDisk := withGovernanceJournalDisk
	withAuditJournalDisk.AuditLogDiskBytes = 1
	if err := withAuditJournalDisk.ValidateReplicationCandidates(); err == nil {
		t.Fatal("Audit journal disk owner did not reserve its additional Recovery claim")
	}
	withAuditJournalDisk.RecoveryClaims = 17
	withAuditJournalDisk.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	if err := withAuditJournalDisk.ValidateReplicationCandidates(); err != nil {
		t.Fatal("HA Audit journal disk owner with a dedicated Recovery claim rejected", err)
	}
	withOrderingJournalDisk := withAuditJournalDisk
	withOrderingJournalDisk.OrderingJournalDiskBytes = 1
	if err := withOrderingJournalDisk.ValidateReplicationCandidates(); err == nil {
		t.Fatal("Ordering journal disk owner did not reserve its additional Recovery claim")
	}
	withOrderingJournalDisk.RecoveryClaims = 18
	withOrderingJournalDisk.RecoveryReserveRAMBytes += resourcebudget.ClaimRAMBytes
	if err := withOrderingJournalDisk.ValidateReplicationCandidates(); err != nil {
		t.Fatal("HA Ordering journal disk owner with a dedicated Recovery claim rejected", err)
	}
	// Only offline commands charge backup/restore workspaces, to their own
	// managers; a serving member holds them beside no replication session.
	withOfflineWorkspaces := b
	withOfflineWorkspaces.BackupWorkspaceDiskBytes = 1
	withOfflineWorkspaces.RestoreWorkspaceDiskBytes = 1
	if err := withOfflineWorkspaces.ValidateReplicationCandidates(); err != nil {
		t.Fatal("offline backup/restore workspaces changed the serving HA Recovery requirement", err)
	}
	for _, shape := range []func(*ResourceBudget){
		func(b *ResourceBudget) { b.ReplicationCandidateRAMBytes = -1 },
		func(b *ResourceBudget) { b.ReplicationCandidateRAMBytes = 0 },
		func(b *ResourceBudget) { b.ReplicationCandidateRAMBytes-- },
		func(b *ResourceBudget) { b.ReplicationCandidateRAMBytes = math.MaxInt64 },
		func(b *ResourceBudget) { b.ReplicationQueueRAMBytes = 0 },
		func(b *ResourceBudget) { b.ReplicationReadRAMBytes = 0 },
		func(b *ResourceBudget) { b.ReplicationQueueRAMBytes = -1 },
		func(b *ResourceBudget) { b.ReplicationQueueRAMBytes-- },
		func(b *ResourceBudget) { b.ReplicationQueueRAMBytes = math.MaxInt64 },
		func(b *ResourceBudget) { b.ReplicationReadRAMBytes = -1 },
		func(b *ResourceBudget) { b.ReplicationReadRAMBytes-- },
		func(b *ResourceBudget) { b.ReplicationReadRAMBytes = math.MaxInt64 },
		func(b *ResourceBudget) { b.ReplicationReaderRAMBytes = 0 },
		func(b *ResourceBudget) { b.ReplicationReaderRAMBytes = -1 },
		func(b *ResourceBudget) { b.ReplicationReaderRAMBytes-- },
		func(b *ResourceBudget) { b.ReplicationReaderRAMBytes = math.MaxInt64 },
		func(b *ResourceBudget) { b.AccountingRecoveryRAMBytes-- },
		func(b *ResourceBudget) { b.AccountingRecoveryRAMBytes = math.MaxInt64 },
		func(b *ResourceBudget) { b.RecoveryClaims-- },
		func(b *ResourceBudget) { b.GovernanceLogRAMBytes = 0 },
		func(b *ResourceBudget) { b.GovernanceLogRAMBytes-- },
		func(b *ResourceBudget) { b.GovernanceLogRAMBytes = math.MaxInt64 },
		func(b *ResourceBudget) { b.RecoveryRAMBytes-- },
		func(b *ResourceBudget) { b.RecoveryReserveRAMBytes-- },
	} {
		copy := b
		shape(&copy)
		if err := copy.ValidateReplicationCandidates(); err == nil {
			t.Fatal("invalid candidate budget accepted", copy)
		}
	}
	// Each component fits individually; only their aggregate crosses the ceiling.
	ceiling := int64((math.MaxInt64 - 12*resourcebudget.ClaimRAMBytes - d - g - au - p - io - a) / 3)
	edge := b
	edge.ReplicationCandidateRAMBytes = ceiling - q - r
	edge.RecoveryRAMBytes = 3*ceiling + d + g + au + p + io + a
	edge.RecoveryReserveRAMBytes = 3*ceiling + d + g + au + p + io + a + 12*resourcebudget.ClaimRAMBytes
	if err := edge.ValidateReplicationCandidates(); err != nil {
		t.Fatal("exact aggregate capacity rejected", err)
	}
	for _, bump := range []func(*ResourceBudget){
		func(b *ResourceBudget) { b.ReplicationCandidateRAMBytes++ },
		func(b *ResourceBudget) { b.ReplicationQueueRAMBytes++ },
		func(b *ResourceBudget) { b.ReplicationReadRAMBytes++ },
		func(b *ResourceBudget) { b.ReplicationReaderRAMBytes += 3 },
	} {
		overflow := edge
		bump(&overflow)
		if err := overflow.ValidateReplicationCandidates(); err == nil {
			t.Fatal("aggregate overflow accepted", overflow)
		}
	}
	encoded, err := yaml.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var round ResourceBudget
	if err := yaml.Unmarshal(encoded, &round); err != nil || round != b {
		t.Fatal("candidate config roundtrip", err, round)
	}
}
func TestResourceBudgetHARequiresCandidateEnvelopeWithoutChangingStandalone(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	// Other replication fields are intentionally invalid; assert the independent
	// resource_budget validation rather than treating them as valid HA config.
	cfg.Replication = &Replication{}
	if err := cfg.Validate(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "replication_candidate_ram_bytes") {
		t.Fatal("HA budget without reconstruction envelope accepted", err)
	}
}

func addProjectionMappingConfigFixture(t *testing.T, b *ResourceBudget) {
	t.Helper()
	ram, err := accountinglimits.ProjectionMappingRAMBytes(b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	b.TotalRAMBytes += 2 * ram
	b.BackgroundRAMBytes += ram
	b.RecoveryRAMBytes += ram
	b.RecoveryReserveRAMBytes += ram
}

func TestResourceBudgetAllProjectionMappingsReserveRecoveryBackgroundAndConcurrentRAM(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig + pressureConfigFixture + ramBudgetConfigFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	b := cfg.ResourceBudget
	b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes, b.GovernanceProjectionDiskBytes = 1, 1, 1
	b.RecoveryClaims, b.MaxClaims = 8, 16
	b.ControlRAMBytes = int64(b.MaxClaims) * resourcebudget.ClaimRAMBytes
	b.TotalRAMBytes += 6 * resourcebudget.ClaimRAMBytes
	b.RecoveryReserveRAMBytes += 3 * resourcebudget.ClaimRAMBytes
	if err := cfg.Validate(LoadOptions{}); err == nil {
		t.Fatal("file Disk capacity was credited as unpaid mapping RAM")
	}
	addProjectionMappingConfigFixture(t, b)
	if err := cfg.Validate(LoadOptions{}); err != nil {
		t.Fatal("calibrated mappings with request/deferred capacity rejected", err)
	}
	for _, shape := range []func(*ResourceBudget){
		func(b *ResourceBudget) { b.BackgroundRAMBytes = 96 << 20 },
		func(b *ResourceBudget) { b.RecoveryRAMBytes = 96 << 20 },
		func(b *ResourceBudget) { b.RecoveryReserveRAMBytes = 96 << 20 },
		func(b *ResourceBudget) { b.TotalRAMBytes = b.RecoveryReserveRAMBytes + (96 << 20) },
		func(b *ResourceBudget) { b.LedgerProjectionDiskBytes = math.MaxInt64 },
	} {
		candidate, copy := cfg, *b
		shape(&copy)
		candidate.ResourceBudget = &copy
		if err := candidate.Validate(LoadOptions{}); err == nil {
			t.Fatal("insufficient/overflowing shared mapping budget accepted", copy)
		}
	}
}
